// Package aggregator manages gRPC connections to all kgpudash-agents,
// aggregates their metric streams, enriches data with Kubernetes pod info,
// and broadcasts updates to WebSocket subscribers.
package aggregator

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/MichaelTrip/kgpudash/internal/server/k8s"
	"github.com/MichaelTrip/kgpudash/internal/server/store"

	pb "github.com/MichaelTrip/kgpudash/internal/proto/gpu"
)

// GPUState is the enriched, current state of a single GPU.
type GPUState struct {
	Index         int     `json:"index"`
	UUID          string  `json:"uuid"`
	Name          string  `json:"name"`
	Vendor        string  `json:"vendor"`
	UtilPercent   float64 `json:"util_percent"`
	VRAMUsedMB    uint64  `json:"vram_used_mb"`
	VRAMTotalMB   uint64  `json:"vram_total_mb"`
	TempCelsius   float64 `json:"temp_celsius"`
	PowerWatts    float64 `json:"power_watts"`
	PodName       string  `json:"pod,omitempty"`
	Namespace     string  `json:"namespace,omitempty"`
	ContainerName string  `json:"container,omitempty"`
}

// NodeState holds the current GPU states for a single node.
type NodeState struct {
	Name string     `json:"name"`
	GPUs []GPUState `json:"gpus"`
}

// Snapshot is the full cluster GPU state broadcast to WebSocket clients.
type Snapshot struct {
	Type      string      `json:"type"` // "snapshot"
	Timestamp int64       `json:"timestamp"`
	Nodes     []NodeState `json:"nodes"`
}

// Subscriber receives Snapshot broadcasts.
type Subscriber chan Snapshot

// Aggregator manages agent connections and state.
type Aggregator struct {
	log        *zap.Logger
	agentPort  string
	intervalMs int64
	mapper     *k8s.Mapper
	store      store.Store

	mu        sync.RWMutex
	nodeState map[string]*NodeState // node name → current state

	subMu sync.RWMutex
	subs  map[Subscriber]struct{}
}

// New creates a new Aggregator.
func New(
	log *zap.Logger,
	agentPort string,
	intervalMs int64,
	mapper *k8s.Mapper,
	st store.Store,
) *Aggregator {
	return &Aggregator{
		log:        log,
		agentPort:  agentPort,
		intervalMs: intervalMs,
		mapper:     mapper,
		store:      st,
		nodeState:  make(map[string]*NodeState),
		subs:       make(map[Subscriber]struct{}),
	}
}

// ConnectAgent dials an agent at the given address and starts streaming metrics.
// It reconnects automatically on failure until ctx is cancelled.
func (a *Aggregator) ConnectAgent(ctx context.Context, nodeName, addr string) {
	go func() {
		for {
			if ctx.Err() != nil {
				return
			}
			a.log.Info("connecting to agent", zap.String("node", nodeName), zap.String("addr", addr))
			if err := a.stream(ctx, nodeName, addr); err != nil {
				if ctx.Err() != nil {
					return
				}
				a.log.Warn("agent stream error, reconnecting in 5s",
					zap.String("node", nodeName), zap.Error(err))
				select {
				case <-ctx.Done():
					return
				case <-time.After(5 * time.Second):
				}
			}
		}
	}()
}

// stream opens a gRPC connection to an agent and processes the metric stream.
func (a *Aggregator) stream(ctx context.Context, nodeName, addr string) error {
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return fmt.Errorf("grpc dial: %w", err)
	}
	defer conn.Close()

	client := pb.NewAgentServiceClient(conn)
	stream, err := client.StreamMetrics(ctx, &pb.StreamRequest{IntervalMs: a.intervalMs})
	if err != nil {
		return fmt.Errorf("StreamMetrics: %w", err)
	}

	for {
		snap, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("recv: %w", err)
		}
		a.handleSnapshot(snap)
	}
}

// handleSnapshot processes an incoming MetricSnapshot from an agent.
func (a *Aggregator) handleSnapshot(snap *pb.MetricSnapshot) {
	ts := time.UnixMilli(snap.GetTimestampMs())
	nodeName := snap.GetNodeName()

	// Enrich with pod info from Kubernetes.
	pods := a.mapper.PodsOnNode(nodeName)
	podsByVendor := groupPodsByVendor(pods)

	gpuStates := make([]GPUState, 0, len(snap.GetGpus()))
	storePoints := make([]store.GPUDataPoint, 0, len(snap.GetGpus()))

	for _, g := range snap.GetGpus() {
		gs := GPUState{
			Index:       int(g.GetIndex()),
			UUID:        g.GetUuid(),
			Name:        g.GetName(),
			Vendor:      g.GetVendor(),
			UtilPercent: g.GetUtilPercent(),
			VRAMUsedMB:  g.GetVramUsedMb(),
			VRAMTotalMB: g.GetVramTotalMb(),
			TempCelsius: g.GetTempCelsius(),
			PowerWatts:  g.GetPowerWatts(),
		}

		// Assign pod info: match by vendor, round-robin across GPU indices.
		if vendorPods := podsByVendor[g.GetVendor()]; len(vendorPods) > 0 {
			podIdx := int(g.GetIndex()) % len(vendorPods)
			p := vendorPods[podIdx]
			gs.PodName = p.PodName
			gs.Namespace = p.Namespace
			gs.ContainerName = p.ContainerName
		}

		gpuStates = append(gpuStates, gs)
		storePoints = append(storePoints, store.GPUDataPoint{
			Time:        ts,
			NodeName:    nodeName,
			GPUIndex:    int(g.GetIndex()),
			GPUUUID:     g.GetUuid(),
			Vendor:      g.GetVendor(),
			UtilPercent: g.GetUtilPercent(),
			VRAMUsedMB:  g.GetVramUsedMb(),
			VRAMTotalMB: g.GetVramTotalMb(),
			TempCelsius: g.GetTempCelsius(),
			PowerWatts:  g.GetPowerWatts(),
		})
	}

	// Update in-memory state.
	a.mu.Lock()
	a.nodeState[nodeName] = &NodeState{Name: nodeName, GPUs: gpuStates}
	a.mu.Unlock()

	// Persist to store.
	if err := a.store.Write(storePoints); err != nil {
		a.log.Warn("store write error", zap.Error(err))
	}

	// Broadcast to WebSocket subscribers.
	a.broadcast()
}

// broadcast sends the current full snapshot to all subscribers.
func (a *Aggregator) broadcast() {
	snap := a.CurrentSnapshot()

	a.subMu.RLock()
	defer a.subMu.RUnlock()

	for sub := range a.subs {
		select {
		case sub <- snap:
		default:
			// Drop if subscriber is slow.
		}
	}
}

// CurrentSnapshot returns the current full cluster state.
func (a *Aggregator) CurrentSnapshot() Snapshot {
	a.mu.RLock()
	defer a.mu.RUnlock()

	nodes := make([]NodeState, 0, len(a.nodeState))
	for _, ns := range a.nodeState {
		nodes = append(nodes, *ns)
	}
	return Snapshot{
		Type:      "snapshot",
		Timestamp: time.Now().UnixMilli(),
		Nodes:     nodes,
	}
}

// Subscribe registers a new WebSocket subscriber channel.
func (a *Aggregator) Subscribe() Subscriber {
	ch := make(Subscriber, 8)
	a.subMu.Lock()
	a.subs[ch] = struct{}{}
	a.subMu.Unlock()
	return ch
}

// Unsubscribe removes a subscriber channel.
func (a *Aggregator) Unsubscribe(ch Subscriber) {
	a.subMu.Lock()
	delete(a.subs, ch)
	a.subMu.Unlock()
	close(ch)
}

// QueryHistory returns historical data for a GPU from the store.
func (a *Aggregator) QueryHistory(nodeName string, gpuIndex int, from, to time.Time) ([]store.GPUDataPoint, error) {
	return a.store.QueryRange(nodeName, gpuIndex, from, to)
}

// groupPodsByVendor groups PodGPUInfo by vendor string.
func groupPodsByVendor(pods []k8s.PodGPUInfo) map[string][]k8s.PodGPUInfo {
	m := make(map[string][]k8s.PodGPUInfo)
	for _, p := range pods {
		m[p.Vendor] = append(m[p.Vendor], p)
	}
	return m
}
