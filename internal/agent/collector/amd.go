package collector

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"go.uber.org/zap"
)

// AMDCollector collects metrics from AMD GPUs.
// Primary: reads sysfs under /sys/class/drm/card*/device/
// Fallback: parses rocm-smi JSON output.
type AMDCollector struct {
	log     *zap.Logger
	cards   []amdCard
	useROCm bool
}

type amdCard struct {
	index int
	path  string // e.g. /sys/class/drm/card0/device
	name  string
}

// NewAMDCollector creates a new AMD collector.
func NewAMDCollector(log *zap.Logger) *AMDCollector {
	return &AMDCollector{log: log}
}

// Detect returns true if AMD GPUs are present via sysfs or rocm-smi.
func (c *AMDCollector) Detect() bool {
	cards := c.discoverSysfs()
	if len(cards) > 0 {
		c.cards = cards
		c.log.Info("AMD GPUs detected via sysfs", zap.Int("count", len(cards)))
		return true
	}

	// Fallback: try rocm-smi
	if _, err := exec.LookPath("rocm-smi"); err == nil {
		out, err := exec.Command("rocm-smi", "--showid", "--json").Output()
		if err == nil && len(out) > 0 {
			c.useROCm = true
			c.log.Info("AMD GPUs detected via rocm-smi fallback")
			return true
		}
	}

	return false
}

// discoverSysfs finds AMD GPU cards under /sys/class/drm.
func (c *AMDCollector) discoverSysfs() []amdCard {
	entries, err := filepath.Glob("/sys/class/drm/card*/device")
	if err != nil {
		return nil
	}

	var cards []amdCard
	idx := 0
	for _, devPath := range entries {
		// Check vendor ID: AMD is 0x1002
		vendorFile := filepath.Join(devPath, "vendor")
		data, err := os.ReadFile(vendorFile)
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(data)) != "0x1002" {
			continue
		}

		name := "AMD GPU"
		if nameData, err := os.ReadFile(filepath.Join(devPath, "product_name")); err == nil {
			name = strings.TrimSpace(string(nameData))
		}

		cards = append(cards, amdCard{
			index: idx,
			path:  devPath,
			name:  name,
		})
		idx++
	}
	return cards
}

// Collect returns a snapshot of all AMD GPU metrics.
func (c *AMDCollector) Collect() ([]GPUInfo, error) {
	if c.useROCm {
		return c.collectROCm()
	}
	return c.collectSysfs()
}

// collectSysfs reads AMD GPU metrics directly from sysfs.
func (c *AMDCollector) collectSysfs() ([]GPUInfo, error) {
	var gpus []GPUInfo
	for _, card := range c.cards {
		info := GPUInfo{
			Index:  card.index,
			Name:   card.name,
			Vendor: "amd",
		}

		// UUID from PCI address
		if link, err := os.Readlink(filepath.Join(card.path, "..")); err == nil {
			info.UUID = "amd-" + filepath.Base(link)
		}

		// GPU utilization
		if data, err := os.ReadFile(filepath.Join(card.path, "gpu_busy_percent")); err == nil {
			if v, err := strconv.ParseFloat(strings.TrimSpace(string(data)), 64); err == nil {
				info.UtilPercent = v
			}
		}

		// VRAM used
		if data, err := os.ReadFile(filepath.Join(card.path, "mem_info_vram_used")); err == nil {
			if v, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64); err == nil {
				info.VRAMUsedMB = v / (1024 * 1024)
			}
		}

		// VRAM total
		if data, err := os.ReadFile(filepath.Join(card.path, "mem_info_vram_total")); err == nil {
			if v, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64); err == nil {
				info.VRAMTotalMB = v / (1024 * 1024)
			}
		}

		// Temperature (hwmon)
		info.TempCelsius = c.readHwmonTemp(card.path)

		// Power draw (hwmon)
		info.PowerWatts = c.readHwmonPower(card.path)

		gpus = append(gpus, info)
	}
	return gpus, nil
}

// readHwmonTemp reads temperature from hwmon sysfs.
func (c *AMDCollector) readHwmonTemp(devPath string) float64 {
	hwmons, _ := filepath.Glob(filepath.Join(devPath, "hwmon", "hwmon*", "temp1_input"))
	for _, f := range hwmons {
		if data, err := os.ReadFile(f); err == nil {
			if v, err := strconv.ParseFloat(strings.TrimSpace(string(data)), 64); err == nil {
				return v / 1000.0 // millidegrees → degrees
			}
		}
	}
	return 0
}

// readHwmonPower reads power draw from hwmon sysfs.
func (c *AMDCollector) readHwmonPower(devPath string) float64 {
	hwmons, _ := filepath.Glob(filepath.Join(devPath, "hwmon", "hwmon*", "power1_average"))
	for _, f := range hwmons {
		if data, err := os.ReadFile(f); err == nil {
			if v, err := strconv.ParseFloat(strings.TrimSpace(string(data)), 64); err == nil {
				return v / 1_000_000.0 // microwatts → watts
			}
		}
	}
	return 0
}

// rocmSMIOutput is a partial representation of rocm-smi JSON output.
type rocmSMIOutput map[string]map[string]string

// collectROCm parses rocm-smi JSON output as a fallback.
func (c *AMDCollector) collectROCm() ([]GPUInfo, error) {
	args := []string{
		"--showuse", "--showmeminfo", "vram",
		"--showtemp", "--showpower", "--showid", "--json",
	}
	out, err := exec.Command("rocm-smi", args...).Output()
	if err != nil {
		return nil, fmt.Errorf("rocm-smi: %w", err)
	}

	var raw rocmSMIOutput
	if err := json.NewDecoder(bytes.NewReader(out)).Decode(&raw); err != nil {
		return nil, fmt.Errorf("rocm-smi json decode: %w", err)
	}

	var gpus []GPUInfo
	idx := 0
	for key, fields := range raw {
		if !strings.HasPrefix(key, "card") {
			continue
		}
		info := GPUInfo{
			Index:  idx,
			Vendor: "amd",
			UUID:   "amd-" + key,
			Name:   fields["Card Series"],
		}
		if v, err := parsePercent(fields["GPU use (%)"]); err == nil {
			info.UtilPercent = v
		}
		if v, err := parseMB(fields["VRAM Total Memory (B)"]); err == nil {
			info.VRAMTotalMB = v
		}
		if v, err := parseMB(fields["VRAM Total Used Memory (B)"]); err == nil {
			info.VRAMUsedMB = v
		}
		if v, err := strconv.ParseFloat(fields["Temperature (Sensor edge) (C)"], 64); err == nil {
			info.TempCelsius = v
		}
		if v, err := strconv.ParseFloat(fields["Average Graphics Package Power (W)"], 64); err == nil {
			info.PowerWatts = v
		}
		gpus = append(gpus, info)
		idx++
	}
	return gpus, nil
}

// Close is a no-op for the AMD collector.
func (c *AMDCollector) Close() error { return nil }

// parsePercent parses a percentage string like "87.5".
func parsePercent(s string) (float64, error) {
	s = strings.TrimSuffix(strings.TrimSpace(s), "%")
	return strconv.ParseFloat(s, 64)
}

// parseMB converts a byte-count string to megabytes.
func parseMB(s string) (uint64, error) {
	v, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, err
	}
	return v / (1024 * 1024), nil
}

// rocmSMILines is used for line-by-line fallback parsing.
type rocmSMILines struct {
	scanner *bufio.Scanner
}
