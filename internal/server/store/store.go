// Package store defines the metrics persistence interface and shared types.
package store

import "time"

// GPUDataPoint is a single historical metric record for one GPU.
type GPUDataPoint struct {
	Time        time.Time
	NodeName    string
	GPUIndex    int
	GPUUUID     string
	Vendor      string
	UtilPercent float64
	VRAMUsedMB  uint64
	VRAMTotalMB uint64
	TempCelsius float64
	PowerWatts  float64
}

// Store is the interface for persisting and querying GPU metrics.
type Store interface {
	// Write persists a batch of data points.
	Write(points []GPUDataPoint) error

	// QueryRange returns historical data for a specific GPU on a node
	// between from and to (inclusive).
	QueryRange(nodeName string, gpuIndex int, from, to time.Time) ([]GPUDataPoint, error)

	// Close releases any resources held by the store.
	Close() error
}
