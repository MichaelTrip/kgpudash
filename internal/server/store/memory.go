package store

import (
	"sync"
	"time"
)

const defaultRingSize = 720 // ~1 hour at 5s intervals

// MemoryStore is an in-memory ring buffer store.
// It keeps the last N data points per (node, gpuIndex) pair.
// Used when storage is disabled — provides enough history for sparklines.
type MemoryStore struct {
	mu       sync.RWMutex
	ringSize int
	rings    map[ringKey][]GPUDataPoint
}

type ringKey struct {
	node     string
	gpuIndex int
}

// NewMemoryStore creates a new in-memory ring buffer store.
func NewMemoryStore(ringSize int) *MemoryStore {
	if ringSize <= 0 {
		ringSize = defaultRingSize
	}
	return &MemoryStore{
		ringSize: ringSize,
		rings:    make(map[ringKey][]GPUDataPoint),
	}
}

// Write appends data points to the ring buffers, evicting oldest entries.
func (m *MemoryStore) Write(points []GPUDataPoint) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, p := range points {
		k := ringKey{node: p.NodeName, gpuIndex: p.GPUIndex}
		ring := m.rings[k]
		ring = append(ring, p)
		if len(ring) > m.ringSize {
			ring = ring[len(ring)-m.ringSize:]
		}
		m.rings[k] = ring
	}
	return nil
}

// QueryRange returns data points for a GPU within the given time range.
func (m *MemoryStore) QueryRange(nodeName string, gpuIndex int, from, to time.Time) ([]GPUDataPoint, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	k := ringKey{node: nodeName, gpuIndex: gpuIndex}
	ring := m.rings[k]

	var result []GPUDataPoint
	for _, p := range ring {
		if !p.Time.Before(from) && !p.Time.After(to) {
			result = append(result, p)
		}
	}
	return result, nil
}

// Close is a no-op for the memory store.
func (m *MemoryStore) Close() error { return nil }
