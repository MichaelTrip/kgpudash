// Package grpcserver implements the gRPC AgentService server that streams
// GPU metrics to the kgpudash-server.
package grpcserver

import (
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/MichaelTrip/kgpudash/internal/agent/collector"
	pb "github.com/MichaelTrip/kgpudash/internal/proto/gpu"
)

// AgentServer implements pb.AgentServiceServer.
type AgentServer struct {
	pb.UnimplementedAgentServiceServer
	log        *zap.Logger
	nodeName   string
	collectors []collector.Collector
}

// New creates a new AgentServer.
func New(log *zap.Logger, nodeName string, collectors []collector.Collector) *AgentServer {
	return &AgentServer{
		log:        log,
		nodeName:   nodeName,
		collectors: collectors,
	}
}

// StreamMetrics implements the server-streaming RPC.
// It collects GPU metrics at the requested interval and sends snapshots
// to the connected kgpudash-server until the context is cancelled.
func (s *AgentServer) StreamMetrics(req *pb.StreamRequest, stream pb.AgentService_StreamMetricsServer) error {
	intervalMs := req.GetIntervalMs()
	if intervalMs <= 0 {
		intervalMs = 5000
	}
	interval := time.Duration(intervalMs) * time.Millisecond

	s.log.Info("StreamMetrics started",
		zap.String("node", s.nodeName),
		zap.Duration("interval", interval),
	)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-stream.Context().Done():
			s.log.Info("StreamMetrics context done", zap.String("node", s.nodeName))
			return nil

		case t := <-ticker.C:
			snapshot, err := s.collect(t)
			if err != nil {
				s.log.Warn("collect error", zap.Error(err))
				return status.Errorf(codes.Internal, "collect: %v", err)
			}
			if err := stream.Send(snapshot); err != nil {
				s.log.Warn("stream send error", zap.Error(err))
				return err
			}
		}
	}
}

// collect gathers metrics from all active collectors and builds a MetricSnapshot.
func (s *AgentServer) collect(t time.Time) (*pb.MetricSnapshot, error) {
	snap := &pb.MetricSnapshot{
		NodeName:    s.nodeName,
		TimestampMs: t.UnixMilli(),
	}

	for _, c := range s.collectors {
		gpus, err := c.Collect()
		if err != nil {
			s.log.Warn("collector error", zap.Error(err))
			continue
		}
		for _, g := range gpus {
			snap.Gpus = append(snap.Gpus, &pb.GPUMetric{
				Index:       int32(g.Index),
				Uuid:        g.UUID,
				Name:        g.Name,
				Vendor:      g.Vendor,
				UtilPercent: g.UtilPercent,
				VramUsedMb:  g.VRAMUsedMB,
				VramTotalMb: g.VRAMTotalMB,
				TempCelsius: g.TempCelsius,
				PowerWatts:  g.PowerWatts,
			})
		}
	}

	return snap, nil
}
