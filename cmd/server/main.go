// Command kgpudash-server aggregates GPU metrics from all kgpudash-agents,
// enriches them with Kubernetes pod information, and serves the web dashboard.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/MichaelTrip/kgpudash/internal/server/aggregator"
	"github.com/MichaelTrip/kgpudash/internal/server/k8s"
	"github.com/MichaelTrip/kgpudash/internal/server/store"
	"github.com/MichaelTrip/kgpudash/internal/server/web"
)

func main() {
	log, _ := zap.NewProduction()
	defer log.Sync()

	// ── Configuration from environment ──────────────────────────────
	httpPort := envOr("SERVER_HTTP_PORT", "8080")
	agentPort := envOr("AGENT_GRPC_PORT", "9090")
	kubeconfig := os.Getenv("KUBECONFIG")
	collectInterval := parseDuration(envOr("COLLECT_INTERVAL", "5s"))
	storageEnabled := os.Getenv("STORAGE_ENABLED") == "true"
	storageDSN := os.Getenv("STORAGE_DSN")

	log.Info("kgpudash-server starting",
		zap.String("http_port", httpPort),
		zap.String("agent_port", agentPort),
		zap.Bool("storage_enabled", storageEnabled),
	)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	// ── Kubernetes mapper ────────────────────────────────────────────
	// Falls back to a no-op mapper when no cluster is reachable so the
	// server can run locally without any Kubernetes configuration.
	mapper := k8s.NewMapperOrNoop(log, kubeconfig)
	go mapper.Start(ctx, 15*time.Second)

	// ── Metrics store ────────────────────────────────────────────────
	var st store.Store
	if storageEnabled {
		if storageDSN == "" {
			log.Fatal("STORAGE_ENABLED=true but STORAGE_DSN is not set")
		}
		ts, err := store.NewTimescaleStore(ctx, storageDSN, log)
		if err != nil {
			log.Fatal("failed to connect to TimescaleDB", zap.Error(err))
		}
		defer ts.Close()
		st = ts
		log.Info("TimescaleDB storage enabled")
	} else {
		st = store.NewMemoryStore(0)
		log.Info("in-memory storage (no persistence)")
	}

	// ── Aggregator ───────────────────────────────────────────────────
	agg := aggregator.New(
		log,
		agentPort,
		collectInterval.Milliseconds(),
		mapper,
		st,
	)

	// ── Discover and connect to agents ───────────────────────────────
	// The aggregator watches for agent pods via the Kubernetes API.
	// We start a goroutine that periodically discovers agent pod IPs
	// and connects to any new ones, cancelling stale connections when
	// a pod IP changes.
	go watchAgentPods(ctx, log, agg, mapper, agentPort)

	// ── Web server ───────────────────────────────────────────────────
	webSrv := web.New(log, agg, fmt.Sprintf(":%s", httpPort))

	go func() {
		<-ctx.Done()
		log.Info("shutting down web server")
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutCancel()
		webSrv.Shutdown(shutCtx)
	}()

	if err := webSrv.Start(); err != nil {
		// http.ErrServerClosed is expected on graceful shutdown.
		log.Info("web server stopped", zap.Error(err))
	}
}

// agentConn tracks the current connection state for one agent node.
type agentConn struct {
	addr   string
	cancel context.CancelFunc
}

// watchAgentPods periodically discovers kgpudash-agent pods via the Kubernetes
// API and connects the aggregator to any new agents. When a pod's IP changes
// (e.g. after a restart), the old connection goroutine is cancelled and a new
// one is started for the updated address.
func watchAgentPods(ctx context.Context, log *zap.Logger, agg *aggregator.Aggregator, mapper *k8s.Mapper, agentPort string) {
	// conns maps node name → current active connection.
	conns := make(map[string]*agentConn)

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	// Run immediately on start.
	discoverAndConnect(ctx, log, agg, mapper, agentPort, conns)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			discoverAndConnect(ctx, log, agg, mapper, agentPort, conns)
		}
	}
}

// discoverAndConnect finds agent pods and connects to new or changed ones.
// It cancels goroutines for nodes whose pod IP has changed.
//
// Priority order:
//  1. NODE_IPS env var — explicit comma-separated list of IPs/hostnames.
//  2. Kubernetes API — list running kgpudash-agent pods and use their pod IPs.
//  3. Localhost fallback — connect to 127.0.0.1 when neither of the above
//     yields any addresses (useful for local development).
func discoverAndConnect(ctx context.Context, log *zap.Logger, agg *aggregator.Aggregator, mapper *k8s.Mapper, agentPort string, conns map[string]*agentConn) {
	// 1. Explicit NODE_IPS override.
	if nodeIPs := os.Getenv("NODE_IPS"); nodeIPs != "" {
		for _, ip := range splitTrim(nodeIPs, ",") {
			if ip == "" {
				continue
			}
			addr := fmt.Sprintf("%s:%s", ip, agentPort)
			connectIfChanged(ctx, log, agg, ip, addr, conns)
		}
		return
	}

	// 2. Auto-discover via Kubernetes API.
	agentPods, err := mapper.ListAgentPodIPs(ctx)
	if err != nil {
		log.Warn("failed to list agent pods from k8s", zap.Error(err))
	}
	if len(agentPods) > 0 {
		for _, p := range agentPods {
			addr := fmt.Sprintf("%s:%s", p.PodIP, agentPort)
			connectIfChanged(ctx, log, agg, p.NodeName, addr, conns)
		}
		return
	}

	// 3. Localhost fallback for local development.
	const localHost = "127.0.0.1"
	addr := fmt.Sprintf("%s:%s", localHost, agentPort)
	connectIfChanged(ctx, log, agg, localHost, addr, conns)
}

// connectIfChanged starts a new agent connection for nodeName/addr if:
//   - there is no existing connection for that node, OR
//   - the address has changed (pod was rescheduled to a new IP).
//
// When the address changes, the old goroutine is cancelled before the new one
// is started, preventing duplicate streaming goroutines per node.
func connectIfChanged(ctx context.Context, log *zap.Logger, agg *aggregator.Aggregator, nodeName, addr string, conns map[string]*agentConn) {
	existing, ok := conns[nodeName]
	if ok && existing.addr == addr {
		// Same address — already connected, nothing to do.
		return
	}
	if ok {
		// Address changed: cancel the old goroutine.
		log.Info("agent IP changed, reconnecting",
			zap.String("node", nodeName),
			zap.String("old", existing.addr),
			zap.String("new", addr))
		existing.cancel()
	}

	nodeCtx, nodeCancel := context.WithCancel(ctx)
	conns[nodeName] = &agentConn{addr: addr, cancel: nodeCancel}

	log.Info("connecting to agent (k8s discovery)",
		zap.String("node", nodeName),
		zap.String("addr", addr))
	agg.ConnectAgent(nodeCtx, nodeName, addr)
}

// ── Helpers ──────────────────────────────────────────────────────────────

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func parseDuration(s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 5 * time.Second
	}
	return d
}

func splitTrim(s, sep string) []string {
	var out []string
	for _, part := range splitStr(s, sep) {
		if t := trimStr(part); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func splitStr(s, sep string) []string {
	var parts []string
	start := 0
	for i := 0; i <= len(s)-len(sep); i++ {
		if s[i:i+len(sep)] == sep {
			parts = append(parts, s[start:i])
			start = i + len(sep)
		}
	}
	parts = append(parts, s[start:])
	return parts
}

func trimStr(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t' || s[0] == '\n' || s[0] == '\r') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t' || s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
