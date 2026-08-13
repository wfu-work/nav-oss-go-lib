package server

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/mem"
)

func collectHostInfo(stateFile string) HostInfo {
	result := HostInfo{
		OperatingSystem: runtime.GOOS,
		Architecture:    runtime.GOARCH,
		CPULogicalCores: runtime.NumCPU(),
	}

	// Collect only fields shown by the console. Avoid host.Info because it also
	// reads optional host identifiers that must not be exposed by this endpoint.
	if platform, _, version, err := host.PlatformInformation(); err == nil {
		result.Platform = firstNonEmpty(platform, result.Platform)
		result.PlatformVersion = firstNonEmpty(version, result.PlatformVersion)
	}
	if version, err := host.KernelVersion(); err == nil {
		result.KernelVersion = firstNonEmpty(version, result.KernelVersion)
	}

	if cores, err := cpu.Counts(false); err == nil && cores > 0 {
		result.CPUPhysicalCores = cores
	}
	if cores, err := cpu.Counts(true); err == nil && cores > 0 {
		result.CPULogicalCores = cores
	}
	if processors, err := cpu.Info(); err == nil {
		for _, processor := range processors {
			if model := strings.TrimSpace(processor.ModelName); model != "" {
				result.CPUModel = model
				break
			}
		}
	}

	if memory, err := mem.VirtualMemory(); err == nil {
		result.MemoryTotalBytes = memory.Total
		result.MemoryUsedBytes = memory.Used
		result.MemoryAvailableBytes = memory.Available
		result.MemoryUsedPercent = memory.UsedPercent
	}

	if usage, err := disk.Usage(existingDiskPath(stateFile)); err == nil {
		result.DiskTotalBytes = usage.Total
		result.DiskUsedBytes = usage.Used
		result.DiskAvailableBytes = usage.Free
		result.DiskUsedPercent = usage.UsedPercent
	}

	return result
}

func existingDiskPath(stateFile string) string {
	path := strings.TrimSpace(stateFile)
	if path == "" {
		return "."
	}
	if absolutePath, err := filepath.Abs(path); err == nil {
		path = absolutePath
	}
	path = filepath.Dir(path)
	for {
		if _, err := os.Stat(path); err == nil {
			return path
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "."
		}
		path = parent
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
