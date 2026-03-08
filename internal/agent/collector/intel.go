package collector

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"go.uber.org/zap"
)

// IntelCollector collects metrics from Intel GPUs (Arc, Xe, integrated).
// Primary: reads i915/xe sysfs under /sys/class/drm/card*/device/
// Fallback: parses xpu-smi output if Intel XPU Manager is installed.
type IntelCollector struct {
	log       *zap.Logger
	cards     []intelCard
	useXPUSMI bool
}

type intelCard struct {
	index  int
	path   string // e.g. /sys/class/drm/card0/device
	name   string
	driver string // "i915" or "xe"
}

// NewIntelCollector creates a new Intel GPU collector.
func NewIntelCollector(log *zap.Logger) *IntelCollector {
	return &IntelCollector{log: log}
}

// Detect returns true if Intel GPUs are present.
func (c *IntelCollector) Detect() bool {
	cards := c.discoverSysfs()
	if len(cards) > 0 {
		c.cards = cards
		c.log.Info("Intel GPUs detected via sysfs", zap.Int("count", len(cards)))
		return true
	}

	// Fallback: try xpu-smi
	if _, err := exec.LookPath("xpu-smi"); err == nil {
		out, err := exec.Command("xpu-smi", "discovery").Output()
		if err == nil && strings.Contains(string(out), "Intel") {
			c.useXPUSMI = true
			c.log.Info("Intel GPUs detected via xpu-smi fallback")
			return true
		}
	}

	return false
}

// discoverSysfs finds Intel GPU cards under /sys/class/drm.
func (c *IntelCollector) discoverSysfs() []intelCard {
	entries, err := filepath.Glob("/sys/class/drm/card*/device")
	if err != nil {
		return nil
	}

	var cards []intelCard
	idx := 0
	for _, devPath := range entries {
		// Check vendor ID: Intel is 0x8086
		vendorFile := filepath.Join(devPath, "vendor")
		data, err := os.ReadFile(vendorFile)
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(data)) != "0x8086" {
			continue
		}

		// Determine driver (i915 or xe)
		driver := "i915"
		driverLink, err := os.Readlink(filepath.Join(devPath, "driver"))
		if err == nil {
			driverName := filepath.Base(driverLink)
			if driverName == "xe" {
				driver = "xe"
			}
		}

		name := "Intel GPU"
		if nameData, err := os.ReadFile(filepath.Join(devPath, "product_name")); err == nil {
			name = strings.TrimSpace(string(nameData))
		}

		cards = append(cards, intelCard{
			index:  idx,
			path:   devPath,
			name:   name,
			driver: driver,
		})
		idx++
	}
	return cards
}

// Collect returns a snapshot of all Intel GPU metrics.
func (c *IntelCollector) Collect() ([]GPUInfo, error) {
	if c.useXPUSMI {
		return c.collectXPUSMI()
	}
	return c.collectSysfs()
}

// collectSysfs reads Intel GPU metrics from sysfs.
func (c *IntelCollector) collectSysfs() ([]GPUInfo, error) {
	var gpus []GPUInfo
	for _, card := range c.cards {
		info := GPUInfo{
			Index:  card.index,
			Name:   card.name,
			Vendor: "intel",
		}

		// UUID from PCI address
		if link, err := os.Readlink(filepath.Join(card.path, "..")); err == nil {
			info.UUID = "intel-" + filepath.Base(link)
		}

		// GPU utilization — try GT0 RC6 residency as a proxy for busy %
		// For xe driver: /sys/class/drm/card*/device/tile0/gt0/freq0/cur_freq
		// For i915: /sys/kernel/debug/dri/*/i915_frequency_info (requires root)
		// Best-effort: read from fdinfo if available
		info.UtilPercent = c.readUtilization(card)

		// VRAM (Local Memory / LMEM) — xe driver exposes this
		info.VRAMUsedMB, info.VRAMTotalMB = c.readVRAM(card)

		// Temperature
		info.TempCelsius = c.readHwmonTemp(card.path)

		// Power
		info.PowerWatts = c.readHwmonPower(card.path)

		gpus = append(gpus, info)
	}
	return gpus, nil
}

// readUtilization attempts to read GPU busy percentage from sysfs.
// It uses act_freq/max_freq as a proxy, clamped to [0, 100].
func (c *IntelCollector) readUtilization(card intelCard) float64 {
	// Try xe driver: act_freq / max_freq * 100
	actPath := filepath.Join(card.path, "tile0", "gt0", "gt_act_freq_mhz")
	maxPath := filepath.Join(card.path, "tile0", "gt0", "gt_max_freq_mhz")

	actData, actErr := os.ReadFile(actPath)
	maxData, maxErr := os.ReadFile(maxPath)
	if actErr == nil && maxErr == nil {
		act, err1 := strconv.ParseFloat(strings.TrimSpace(string(actData)), 64)
		max, err2 := strconv.ParseFloat(strings.TrimSpace(string(maxData)), 64)
		if err1 == nil && err2 == nil && max > 0 {
			pct := (act / max) * 100.0
			if pct > 100 {
				pct = 100
			}
			if pct < 0 {
				pct = 0
			}
			return pct
		}
	}

	// Try i915 driver: cur_freq / max_freq * 100
	curPatterns := []string{
		filepath.Join(card.path, "gt", "gt0", "rps_cur_freq_mhz"),
		filepath.Join(card.path, "gt0_cur_freq_mhz"),
	}
	maxPatterns := []string{
		filepath.Join(card.path, "gt", "gt0", "rps_max_freq_mhz"),
		filepath.Join(card.path, "gt0_max_freq_mhz"),
	}
	for i := range curPatterns {
		curData, curErr := os.ReadFile(curPatterns[i])
		mxData, mxErr := os.ReadFile(maxPatterns[i])
		if curErr == nil && mxErr == nil {
			cur, err1 := strconv.ParseFloat(strings.TrimSpace(string(curData)), 64)
			mx, err2 := strconv.ParseFloat(strings.TrimSpace(string(mxData)), 64)
			if err1 == nil && err2 == nil && mx > 0 {
				pct := (cur / mx) * 100.0
				if pct > 100 {
					pct = 100
				}
				if pct < 0 {
					pct = 0
				}
				return pct
			}
		}
	}

	return 0
}

// readVRAM reads local memory (LMEM) stats for discrete Intel GPUs.
func (c *IntelCollector) readVRAM(card intelCard) (used, total uint64) {
	// xe driver exposes: /sys/class/drm/card*/device/tile0/memory/local/total
	//                    /sys/class/drm/card*/device/tile0/memory/local/used
	totalPath := filepath.Join(card.path, "tile0", "memory", "local", "total")
	usedPath := filepath.Join(card.path, "tile0", "memory", "local", "used")

	if data, err := os.ReadFile(totalPath); err == nil {
		if v, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64); err == nil {
			total = v / (1024 * 1024)
		}
	}
	if data, err := os.ReadFile(usedPath); err == nil {
		if v, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64); err == nil {
			used = v / (1024 * 1024)
		}
	}
	return
}

// readHwmonTemp reads temperature from hwmon sysfs.
func (c *IntelCollector) readHwmonTemp(devPath string) float64 {
	hwmons, _ := filepath.Glob(filepath.Join(devPath, "hwmon", "hwmon*", "temp1_input"))
	for _, f := range hwmons {
		if data, err := os.ReadFile(f); err == nil {
			if v, err := strconv.ParseFloat(strings.TrimSpace(string(data)), 64); err == nil {
				return v / 1000.0
			}
		}
	}
	return 0
}

// readHwmonPower reads power draw from hwmon sysfs.
func (c *IntelCollector) readHwmonPower(devPath string) float64 {
	hwmons, _ := filepath.Glob(filepath.Join(devPath, "hwmon", "hwmon*", "power1_input"))
	for _, f := range hwmons {
		if data, err := os.ReadFile(f); err == nil {
			if v, err := strconv.ParseFloat(strings.TrimSpace(string(data)), 64); err == nil {
				return v / 1_000_000.0
			}
		}
	}
	return 0
}

// xpuSMIDevice represents one device in xpu-smi dump output.
type xpuSMIDevice struct {
	id                  int
	name                string
	util                float64
	vramUsed, vramTotal uint64
	temp, power         float64
}

// collectXPUSMI parses xpu-smi dump output as a fallback.
func (c *IntelCollector) collectXPUSMI() ([]GPUInfo, error) {
	// xpu-smi dump -d all -m 0,1,2,3,5 outputs CSV-like data
	out, err := exec.Command("xpu-smi", "dump", "-d", "-1", "-m", "0,1,2,3,5", "-i", "1", "-n", "1").Output()
	if err != nil {
		return nil, fmt.Errorf("xpu-smi dump: %w", err)
	}

	devices := parseXPUSMI(out)
	gpus := make([]GPUInfo, 0, len(devices))
	for _, d := range devices {
		gpus = append(gpus, GPUInfo{
			Index:       d.id,
			UUID:        fmt.Sprintf("intel-xpu-%d", d.id),
			Name:        d.name,
			Vendor:      "intel",
			UtilPercent: d.util,
			VRAMUsedMB:  d.vramUsed,
			VRAMTotalMB: d.vramTotal,
			TempCelsius: d.temp,
			PowerWatts:  d.power,
		})
	}
	return gpus, nil
}

// parseXPUSMI parses xpu-smi CSV dump output.
func parseXPUSMI(data []byte) []xpuSMIDevice {
	var devices []xpuSMIDevice
	scanner := bufio.NewScanner(bytes.NewReader(data))
	var headers []string
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, ",")
		if headers == nil {
			headers = parts
			continue
		}
		if len(parts) < len(headers) {
			continue
		}
		row := make(map[string]string, len(headers))
		for i, h := range headers {
			row[strings.TrimSpace(h)] = strings.TrimSpace(parts[i])
		}
		d := xpuSMIDevice{}
		if v, err := strconv.Atoi(row["DeviceId"]); err == nil {
			d.id = v
		}
		d.name = row["DeviceName"]
		if v, err := strconv.ParseFloat(row["GPU Utilization (%)"], 64); err == nil {
			d.util = v
		}
		if v, err := strconv.ParseUint(row["GPU Memory Used (MiB)"], 10, 64); err == nil {
			d.vramUsed = v
		}
		if v, err := strconv.ParseUint(row["GPU Memory Size (MiB)"], 10, 64); err == nil {
			d.vramTotal = v
		}
		if v, err := strconv.ParseFloat(row["GPU Temperature (Celsius)"], 64); err == nil {
			d.temp = v
		}
		if v, err := strconv.ParseFloat(row["GPU Power (W)"], 64); err == nil {
			d.power = v
		}
		devices = append(devices, d)
	}
	return devices
}

// Close is a no-op for the Intel collector.
func (c *IntelCollector) Close() error { return nil }
