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
	mapper, err := k8s.NewMapper(log, kubeconfig)
	if err != nil {
		log.Fatal("failed to create k8s mapper", zap.Error(err))
	}
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
	// and connects to any new ones.
	go watchAgentPods(ctx, log, agg, agentPort)

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

// watchAgentPods periodically discovers kgpudash-agent pods via the Kubernetes
// API and connects the aggregator to any new agents.
func watchAgentPods(ctx context.Context, log *zap.Logger, agg *aggregator.Aggregator, agentPort string) {
	// Track which nodes we've already connected to.
	connected := make(map[string]bool)

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	// Run immediately on start.
	discoverAndConnect(ctx, log, agg, agentPort, connected)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			discoverAndConnect(ctx, log, agg, agentPort, connected)
		}
	}
}

// discoverAndConnect finds agent pods and connects to new ones.
// In a real deployment the agent pod IPs come from the Kubernetes API.
// Here we use the NODE_IPS environment variable as a simple bootstrap
// mechanism (comma-separated list of node IPs or hostnames).
func discoverAndConnect(ctx context.Context, log *zap.Logger, agg *aggregator.Aggregator, agentPort string, connected map[string]bool) {
	nodeIPs := os.Getenv("NODE_IPS")
	if nodeIPs == "" {
		return
	}

	for _, ip := range splitTrim(nodeIPs, ",") {
		if ip == "" || connected[ip] {
			continue
		}
		addr := fmt.Sprintf("%s:%s", ip, agentPort)
		log.Info("connecting to agent", zap.String("addr", addr))
		agg.ConnectAgent(ctx, ip, addr)
		connected[ip] = true
	}
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
