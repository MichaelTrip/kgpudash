// Command kgpudash-agent runs on every GPU node as a DaemonSet.
// It detects GPU hardware, collects metrics, and streams them to the
// kgpudash-server via gRPC.
package main

import (
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"

	"go.uber.org/zap"
	"google.golang.org/grpc"

	"github.com/MichaelTrip/kgpudash/internal/agent/collector"
	"github.com/MichaelTrip/kgpudash/internal/agent/grpcserver"
	pb "github.com/MichaelTrip/kgpudash/internal/proto/gpu"
)

func main() {
	log, _ := zap.NewProduction()
	defer log.Sync()

	port := envOr("AGENT_GRPC_PORT", "9090")
	nodeName := envOr("NODE_NAME", mustHostname())

	log.Info("kgpudash-agent starting",
		zap.String("node", nodeName),
		zap.String("port", port),
	)

	// Auto-detect GPU vendors present on this node.
	candidates := []collector.Collector{
		collector.NewNvidiaCollector(log),
		collector.NewAMDCollector(log),
		collector.NewIntelCollector(log),
	}
	active := collector.AutoDetect(candidates)
	if len(active) == 0 {
		log.Warn("No GPU collectors detected on this node — agent will stream empty snapshots")
	}

	// Start gRPC server.
	lis, err := net.Listen("tcp", fmt.Sprintf(":%s", port))
	if err != nil {
		log.Fatal("failed to listen", zap.Error(err))
	}

	grpcSrv := grpc.NewServer()
	agentSrv := grpcserver.New(log, nodeName, active)
	pb.RegisterAgentServiceServer(grpcSrv, agentSrv)

	// Graceful shutdown on SIGTERM / SIGINT.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-quit
		log.Info("shutting down agent gRPC server")
		grpcSrv.GracefulStop()
		for _, c := range active {
			c.Close()
		}
	}()

	log.Info("agent gRPC server listening", zap.String("addr", lis.Addr().String()))
	if err := grpcSrv.Serve(lis); err != nil {
		log.Fatal("gRPC serve error", zap.Error(err))
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func mustHostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}
