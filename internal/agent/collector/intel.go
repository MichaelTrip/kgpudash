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
	"time"

	"go.uber.org/zap"
)

// IntelCollector collects metrics from Intel GPUs (Arc, Xe, integrated).
// Primary: reads i915/xe sysfs under /sys/class/drm/card*/device/
// Fallback: parses xpu-smi output if Intel XPU Manager is installed.
type IntelCollector struct {
	log       *zap.Logger
	cards     []intelCard
	useXPUSMI bool

	// For energy_uj delta-based power calculation
	lastEnergyUJ map[string]uint64
	lastEnergyTs map[string]time.Time
}

type intelCard struct {
	index    int
	path     string // e.g. /sys/class/drm/card0/device
	cardPath string // e.g. /sys/class/drm/card0  (freq files live here on i915)
	name     string
	driver   string // "i915" or "xe"
}

// NewIntelCollector creates a new Intel GPU collector.
func NewIntelCollector(log *zap.Logger) *IntelCollector {
	return &IntelCollector{
		log:          log,
		lastEnergyUJ: make(map[string]uint64),
		lastEnergyTs: make(map[string]time.Time),
	}
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

		// cardPath is the parent of devPath: /sys/class/drm/card0
		cardPath := filepath.Dir(devPath)

		cards = append(cards, intelCard{
			index:    idx,
			path:     devPath,
			cardPath: cardPath,
			name:     name,
			driver:   driver,
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
		info.UtilPercent = c.readUtilization(card)

		// VRAM (Local Memory / LMEM) — xe driver exposes this; i915 via debugfs
		info.VRAMUsedMB, info.VRAMTotalMB = c.readVRAM(card)

		// Temperature
		info.TempCelsius = c.readHwmonTemp(card)

		// Power
		info.PowerWatts = c.readHwmonPower(card)

		gpus = append(gpus, info)
	}
	return gpus, nil
}

// readUtilization attempts to read GPU busy percentage from sysfs.
// It uses act_freq/max_freq as a proxy, clamped to [0, 100].
//
// Path layout varies by driver and kernel version:
//   - xe driver (newer):  card.path/tile0/gt0/gt_act_freq_mhz
//   - i915 (newer):       card.path/gt/gt0/rps_cur_freq_mhz
//   - i915 (Skylake era): card.cardPath/gt_act_freq_mhz  (directly under /sys/class/drm/cardN)
func (c *IntelCollector) readUtilization(card intelCard) float64 {
	type freqPair struct{ cur, max string }
	candidates := []freqPair{
		// xe driver
		{
			filepath.Join(card.path, "tile0", "gt0", "gt_act_freq_mhz"),
			filepath.Join(card.path, "tile0", "gt0", "gt_max_freq_mhz"),
		},
		// i915 newer kernels (device/gt/gt0/)
		{
			filepath.Join(card.path, "gt", "gt0", "rps_cur_freq_mhz"),
			filepath.Join(card.path, "gt", "gt0", "rps_max_freq_mhz"),
		},
		// i915 Skylake/older: files live directly under /sys/class/drm/cardN
		{
			filepath.Join(card.cardPath, "gt_act_freq_mhz"),
			filepath.Join(card.cardPath, "gt_max_freq_mhz"),
		},
		{
			filepath.Join(card.cardPath, "gt_cur_freq_mhz"),
			filepath.Join(card.cardPath, "gt_max_freq_mhz"),
		},
	}

	for _, p := range candidates {
		curData, curErr := os.ReadFile(p.cur)
		maxData, maxErr := os.ReadFile(p.max)
		if curErr != nil || maxErr != nil {
			continue
		}
		cur, err1 := strconv.ParseFloat(strings.TrimSpace(string(curData)), 64)
		max, err2 := strconv.ParseFloat(strings.TrimSpace(string(maxData)), 64)
		if err1 != nil || err2 != nil || max <= 0 {
			continue
		}
		pct := (cur / max) * 100.0
		if pct > 100 {
			pct = 100
		}
		if pct < 0 {
			pct = 0
		}
		return pct
	}

	return 0
}

// readVRAM reads local memory (LMEM) stats for discrete Intel GPUs.
// Tries multiple paths in order of preference:
//  1. xe driver sysfs: tile0/memory/local/{total,used}
//  2. i915 debugfs:    /sys/kernel/debug/dri/N/i915_gem_objects (requires root)
//  3. i915 sysfs gem:  card.path/drm/card*/mem_info_vram_{total,used}
//  4. hwmon lmem:      card.path/hwmon/hwmon*/in0_{input,max} (some Arc cards)
func (c *IntelCollector) readVRAM(card intelCard) (used, total uint64) {
	// 1. xe driver: /sys/class/drm/card*/device/tile0/memory/local/{total,used}
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
	if total > 0 {
		return
	}

	// 2. i915 sysfs mem_info (kernel 6.2+ exposes under drm client stats)
	//    /sys/class/drm/card*/device/drm/card*/clients/*/mem_info_vram_used
	//    Aggregate across all clients for total used.
	clientsGlob := filepath.Join(card.cardPath, "clients", "*", "mem_info_vram_used")
	if clients, _ := filepath.Glob(clientsGlob); len(clients) > 0 {
		var sumUsed uint64
		for _, f := range clients {
			if data, err := os.ReadFile(f); err == nil {
				if v, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64); err == nil {
					sumUsed += v
				}
			}
		}
		used = sumUsed / (1024 * 1024)
		// Try to get total from the first client's vram_total sibling
		if len(clients) > 0 {
			totalFile := strings.Replace(clients[0], "mem_info_vram_used", "mem_info_vram_total", 1)
			if data, err := os.ReadFile(totalFile); err == nil {
				if v, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64); err == nil {
					total = v / (1024 * 1024)
				}
			}
		}
		if total > 0 {
			return
		}
	}

	// 3. i915 debugfs (requires root / CAP_SYS_ADMIN)
	//    /sys/kernel/debug/dri/<N>/i915_gem_objects
	//    Look for "Total" line: "Total 1234 objects, 567890 bytes"
	cardNum := filepath.Base(card.cardPath) // e.g. "card0"
	cardNum = strings.TrimPrefix(cardNum, "card")
	debugPath := fmt.Sprintf("/sys/kernel/debug/dri/%s/i915_gem_objects", cardNum)
	if data, err := os.ReadFile(debugPath); err == nil {
		scanner := bufio.NewScanner(bytes.NewReader(data))
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "Total") {
				// "Total N objects, M bytes, P purgeable"
				fields := strings.Fields(line)
				for i, f := range fields {
					if f == "bytes," || f == "bytes" {
						if i > 0 {
							if v, err := strconv.ParseUint(fields[i-1], 10, 64); err == nil {
								used = v / (1024 * 1024)
							}
						}
					}
				}
			}
		}
	}

	// 4. Try reading VRAM size from PCI resource (gives total VRAM for discrete GPUs)
	//    /sys/class/drm/card*/device/resource (BAR2 is typically VRAM on discrete)
	if total == 0 {
		resourcePath := filepath.Join(card.path, "resource")
		if data, err := os.ReadFile(resourcePath); err == nil {
			lines := strings.Split(string(data), "\n")
			// BAR2 is line index 4 (0-indexed), format: "start end flags"
			if len(lines) > 4 {
				fields := strings.Fields(lines[4])
				if len(fields) >= 2 {
					start, err1 := strconv.ParseUint(strings.TrimPrefix(fields[0], "0x"), 16, 64)
					end, err2 := strconv.ParseUint(strings.TrimPrefix(fields[1], "0x"), 16, 64)
					if err1 == nil && err2 == nil && end > start {
						total = (end - start + 1) / (1024 * 1024)
					}
				}
			}
		}
	}

	return
}

// readHwmonTemp reads temperature from hwmon sysfs.
// Checks both device/hwmon and card-level hwmon paths.
func (c *IntelCollector) readHwmonTemp(card intelCard) float64 {
	patterns := []string{
		filepath.Join(card.path, "hwmon", "hwmon*", "temp1_input"),
		filepath.Join(card.cardPath, "device", "hwmon", "hwmon*", "temp1_input"),
		// coretemp fallback via /sys/class/hwmon
		"/sys/class/hwmon/hwmon*/temp1_input",
	}
	for _, pat := range patterns {
		hwmons, _ := filepath.Glob(pat)
		for _, f := range hwmons {
			if data, err := os.ReadFile(f); err == nil {
				if v, err := strconv.ParseFloat(strings.TrimSpace(string(data)), 64); err == nil && v > 0 {
					return v / 1000.0
				}
			}
		}
	}
	return 0
}

// readHwmonPower reads power draw from hwmon sysfs.
// Tries power1_input (instantaneous µW), then falls back to
// computing watts from the delta of energy_uj (µJ counter).
func (c *IntelCollector) readHwmonPower(card intelCard) float64 {
	// Patterns for instantaneous power (µW)
	powerPatterns := []string{
		filepath.Join(card.path, "hwmon", "hwmon*", "power1_input"),
		filepath.Join(card.cardPath, "device", "hwmon", "hwmon*", "power1_input"),
		// Some Arc/Xe cards expose power under a different index
		filepath.Join(card.path, "hwmon", "hwmon*", "power2_input"),
		filepath.Join(card.cardPath, "device", "hwmon", "hwmon*", "power2_input"),
	}
	for _, pat := range powerPatterns {
		hwmons, _ := filepath.Glob(pat)
		for _, f := range hwmons {
			if data, err := os.ReadFile(f); err == nil {
				if v, err := strconv.ParseFloat(strings.TrimSpace(string(data)), 64); err == nil && v > 0 {
					return v / 1_000_000.0
				}
			}
		}
	}

	// Fallback: energy_uj counter (µJ) — compute delta watts
	// energy_uj is a monotonically increasing counter; watts = ΔµJ / Δt_µs
	energyPatterns := []string{
		filepath.Join(card.path, "hwmon", "hwmon*", "energy1_input"),
		filepath.Join(card.cardPath, "device", "hwmon", "hwmon*", "energy1_input"),
		// RAPL-style via powercap (integrated GPUs share package energy)
		"/sys/class/powercap/intel-rapl:*/energy_uj",
		"/sys/class/powercap/intel-rapl:*:*/energy_uj",
	}
	now := time.Now()
	key := fmt.Sprintf("card%d", card.index)

	for _, pat := range energyPatterns {
		files, _ := filepath.Glob(pat)
		for _, f := range files {
			data, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			uj, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
			if err != nil {
				continue
			}

			fileKey := key + ":" + f
			if prev, ok := c.lastEnergyUJ[fileKey]; ok {
				dt := now.Sub(c.lastEnergyTs[fileKey]).Seconds()
				if dt > 0 && uj >= prev {
					watts := float64(uj-prev) / 1_000_000.0 / dt
					c.lastEnergyUJ[fileKey] = uj
					c.lastEnergyTs[fileKey] = now
					if watts > 0 && watts < 1000 { // sanity check
						return watts
					}
				}
			}
			c.lastEnergyUJ[fileKey] = uj
			c.lastEnergyTs[fileKey] = now
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
