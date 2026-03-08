// Package collector defines the GPU metric collection interface and shared types.
package collector

// GPUInfo holds a point-in-time snapshot of a single GPU device.
type GPUInfo struct {
	Index       int
	UUID        string
	Name        string
	Vendor      string // "nvidia" | "amd" | "intel"
	UtilPercent float64
	VRAMUsedMB  uint64
	VRAMTotalMB uint64
	TempCelsius float64
	PowerWatts  float64
}

// Collector is the interface every GPU vendor backend must implement.
type Collector interface {
	// Detect returns true if this vendor's GPUs are present on the host.
	Detect() bool

	// Collect returns a snapshot of all GPUs managed by this collector.
	Collect() ([]GPUInfo, error)

	// Close releases any resources held by the collector.
	Close() error
}

// AutoDetect tries each collector in order and returns the first one whose
// Detect() returns true. Multiple collectors may be active if a node has
// mixed GPU vendors (rare but possible).
func AutoDetect(candidates []Collector) []Collector {
	var active []Collector
	for _, c := range candidates {
		if c.Detect() {
			active = append(active, c)
		}
	}
	return active
}
