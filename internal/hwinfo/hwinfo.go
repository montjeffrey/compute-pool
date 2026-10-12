// Package hwinfo describes a device's hardware: its CPU, cores and memory.
// pool setup uses it to recommend how much of a device to lend.
package hwinfo

import (
	"runtime"
	"strings"
)

// Specs describes a device's processor and memory. Fields the platform can't
// report are left at their zero value.
type Specs struct {
	CPUModel    string `json:"cpu_model,omitempty"`
	LogicalCPUs int    `json:"logical_cpus"`
	// PhysicalCores counts cores rather than hardware threads.
	PhysicalCores int `json:"physical_cores,omitempty"`
	// PerfCores and EffCores split PhysicalCores on CPUs that mix
	// performance and efficiency cores; both are 0 on other CPUs.
	PerfCores int   `json:"perf_cores,omitempty"`
	EffCores  int   `json:"eff_cores,omitempty"`
	MemoryMB  int64 `json:"memory_mb,omitempty"`
}

// Hybrid reports whether the CPU mixes performance and efficiency cores.
func (s Specs) Hybrid() bool { return s.PerfCores > 0 && s.EffCores > 0 }

// Detect reads this device's specs.
func Detect() Specs {
	s := detect()
	s.LogicalCPUs = runtime.NumCPU()
	s.CPUModel = strings.Join(strings.Fields(s.CPUModel), " ")
	if s.PhysicalCores > s.LogicalCPUs {
		// Only possible if the process is limited to some CPUs; the core
		// counts then describe the machine, not what this process can use.
		s.PhysicalCores, s.PerfCores, s.EffCores = 0, 0, 0
	}
	return s
}

// splitClasses turns one performance class per core (higher is faster) into
// counts of performance and efficiency cores. Cores of a single class are
// neither: the CPU isn't hybrid, so both counts are 0.
func splitClasses(classes []int) (perf, eff int) {
	if len(classes) == 0 {
		return 0, 0
	}
	top := classes[0]
	for _, c := range classes {
		top = max(top, c)
	}
	for _, c := range classes {
		if c == top {
			perf++
		} else {
			eff++
		}
	}
	if eff == 0 {
		return 0, 0
	}
	return perf, eff
}
