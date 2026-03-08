package collector

import (
	"bufio"
	"bytes"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"go.uber.org/zap"
)

// NvidiaCollector collects metrics from NVIDIA GPUs.
// It tries NVML first (no subprocess) and falls back to nvidia-smi CSV parsing.
type NvidiaCollector struct {
	log      *zap.Logger
	useNVML  bool
	nvmlInit bool
}

// NewNvidiaCollector creates a new NVIDIA collector.
func NewNvidiaCollector(log *zap.Logger) *NvidiaCollector {
	return &NvidiaCollector{log: log}
}

// Detect returns true if NVIDIA GPUs are present (via NVML or nvidia-smi).
func (c *NvidiaCollector) Detect() bool {
	// Try NVML first.
	ret := nvml.Init()
	if ret == nvml.SUCCESS {
		count, ret2 := nvml.DeviceGetCount()
		if ret2 == nvml.SUCCESS && count > 0 {
			c.useNVML = true
			c.nvmlInit = true
			c.log.Info("NVIDIA NVML detected", zap.Int("gpu_count", count))
			return true
		}
		nvml.Shutdown()
	}

	// Fallback: check if nvidia-smi is available.
	if _, err := exec.LookPath("nvidia-smi"); err == nil {
		out, err := exec.Command("nvidia-smi", "-L").Output()
		if err == nil && len(out) > 0 {
			c.useNVML = false
			c.log.Info("NVIDIA detected via nvidia-smi fallback")
			return true
		}
	}

	return false
}

// Collect returns a snapshot of all NVIDIA GPU metrics.
func (c *NvidiaCollector) Collect() ([]GPUInfo, error) {
	if c.useNVML {
		return c.collectNVML()
	}
	return c.collectSMI()
}

// collectNVML uses the NVML library directly (preferred, no subprocess).
func (c *NvidiaCollector) collectNVML() ([]GPUInfo, error) {
	count, ret := nvml.DeviceGetCount()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("nvml DeviceGetCount: %v", nvml.ErrorString(ret))
	}

	gpus := make([]GPUInfo, 0, count)
	for i := 0; i < count; i++ {
		dev, ret := nvml.DeviceGetHandleByIndex(i)
		if ret != nvml.SUCCESS {
			c.log.Warn("nvml DeviceGetHandleByIndex failed", zap.Int("index", i), zap.String("err", nvml.ErrorString(ret)))
			continue
		}

		info := GPUInfo{
			Index:  i,
			Vendor: "nvidia",
		}

		if uuid, ret := dev.GetUUID(); ret == nvml.SUCCESS {
			info.UUID = uuid
		}
		if name, ret := dev.GetName(); ret == nvml.SUCCESS {
			info.Name = name
		}
		if util, ret := dev.GetUtilizationRates(); ret == nvml.SUCCESS {
			info.UtilPercent = float64(util.Gpu)
		}
		if mem, ret := dev.GetMemoryInfo(); ret == nvml.SUCCESS {
			info.VRAMUsedMB = mem.Used / (1024 * 1024)
			info.VRAMTotalMB = mem.Total / (1024 * 1024)
		}
		if temp, ret := dev.GetTemperature(nvml.TEMPERATURE_GPU); ret == nvml.SUCCESS {
			info.TempCelsius = float64(temp)
		}
		if power, ret := dev.GetPowerUsage(); ret == nvml.SUCCESS {
			info.PowerWatts = float64(power) / 1000.0 // mW → W
		}

		gpus = append(gpus, info)
	}
	return gpus, nil
}

// collectSMI parses nvidia-smi CSV output as a fallback.
func (c *NvidiaCollector) collectSMI() ([]GPUInfo, error) {
	args := []string{
		"--query-gpu=index,uuid,name,utilization.gpu,memory.used,memory.total,temperature.gpu,power.draw",
		"--format=csv,noheader,nounits",
	}
	out, err := exec.Command("nvidia-smi", args...).Output()
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi: %w", err)
	}

	var gpus []GPUInfo
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		parts := strings.Split(line, ", ")
		if len(parts) < 8 {
			continue
		}

		idx, _ := strconv.Atoi(strings.TrimSpace(parts[0]))
		util, _ := strconv.ParseFloat(strings.TrimSpace(parts[3]), 64)
		vramUsed, _ := strconv.ParseUint(strings.TrimSpace(parts[4]), 10, 64)
		vramTotal, _ := strconv.ParseUint(strings.TrimSpace(parts[5]), 10, 64)
		temp, _ := strconv.ParseFloat(strings.TrimSpace(parts[6]), 64)
		power, _ := strconv.ParseFloat(strings.TrimSpace(parts[7]), 64)

		gpus = append(gpus, GPUInfo{
			Index:       idx,
			UUID:        strings.TrimSpace(parts[1]),
			Name:        strings.TrimSpace(parts[2]),
			Vendor:      "nvidia",
			UtilPercent: util,
			VRAMUsedMB:  vramUsed,
			VRAMTotalMB: vramTotal,
			TempCelsius: temp,
			PowerWatts:  power,
		})
	}
	return gpus, scanner.Err()
}

// Close shuts down NVML if it was initialised.
func (c *NvidiaCollector) Close() error {
	if c.nvmlInit {
		nvml.Shutdown()
		c.nvmlInit = false
	}
	return nil
}
