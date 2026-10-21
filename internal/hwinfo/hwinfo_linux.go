package hwinfo

import "os"

func detect() Specs {
	var s Specs
	if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		s.CPUModel = cpuInfoModel(string(b))
	}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		s.MemoryMB = memInfoMB(string(b))
	}
	s.PhysicalCores, s.PerfCores, s.EffCores = sysfsCores("/sys")
	return s
}
