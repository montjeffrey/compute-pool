package hwinfo

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"math/bits"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// The parsers below take their input as arguments rather than reading the
// system, so tests can run each platform's format on any platform.

// relationProcessorCore is LOGICAL_PROCESSOR_RELATIONSHIP's RelationProcessorCore.
const relationProcessorCore = 0

// parseProcessorCores reads the SYSTEM_LOGICAL_PROCESSOR_INFORMATION_EX
// records that Windows' GetLogicalProcessorInformationEx returns and gives
// one EfficiencyClass per core. ptrSize is the size of KAFFINITY.
func parseProcessorCores(buf []byte, ptrSize int) ([]int, error) {
	const (
		headerSize = 8  // Relationship, Size
		groupCount = 30 // offset of PROCESSOR_RELATIONSHIP.GroupCount
	)
	var classes []int
	for off := 0; off < len(buf); {
		if len(buf)-off < headerSize {
			return nil, errors.New("truncated processor record")
		}
		rel := binary.LittleEndian.Uint32(buf[off:])
		size := int(binary.LittleEndian.Uint32(buf[off+4:]))
		if size < headerSize || off+size > len(buf) {
			return nil, fmt.Errorf("bad processor record size %d", size)
		}
		if rel == relationProcessorCore {
			rec := buf[off : off+size]
			if len(rec) < groupCount+2 {
				return nil, errors.New("truncated core record")
			}
			// Each GROUP_AFFINITY is a KAFFINITY mask, a group number and
			// padding; the mask says which logical processors the core has.
			groups := int(binary.LittleEndian.Uint16(rec[groupCount:]))
			threads := 0
			for g, at := 0, groupCount+2; g < groups; g++ {
				at = (at + ptrSize - 1) / ptrSize * ptrSize
				if at+ptrSize > len(rec) {
					return nil, errors.New("truncated group affinity")
				}
				if ptrSize == 8 {
					threads += bits.OnesCount64(binary.LittleEndian.Uint64(rec[at:]))
				} else {
					threads += bits.OnesCount32(binary.LittleEndian.Uint32(rec[at:]))
				}
				at += ptrSize + 8
			}
			if threads > 0 {
				classes = append(classes, int(rec[headerSize+1])) // EfficiencyClass
			}
		}
		off += size
	}
	return classes, nil
}

// cpuInfoModel returns the processor name from Linux's /proc/cpuinfo. ARM
// systems often have no "model name"; the board or SoC name is used then.
func cpuInfoModel(data string) string {
	values := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(data))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if ok && v != "" {
			if _, seen := values[k]; !seen {
				values[k] = v
			}
		}
	}
	for _, k := range []string{"model name", "cpu model", "Hardware", "Model"} {
		if v := values[k]; v != "" {
			return v
		}
	}
	return ""
}

// memInfoMB returns MemTotal from Linux's /proc/meminfo, or 0.
func memInfoMB(data string) int64 {
	sc := bufio.NewScanner(strings.NewReader(data))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) >= 2 && f[0] == "MemTotal:" {
			kb, err := strconv.ParseInt(f[1], 10, 64)
			if err == nil {
				return kb / 1024
			}
		}
	}
	return 0
}

// parseCPUList expands a Linux CPU list such as "0-3,8,10-11".
func parseCPUList(s string) ([]int, error) {
	var cpus []int
	for _, part := range strings.Split(strings.TrimSpace(s), ",") {
		if part == "" {
			continue
		}
		lo, hi, isRange := strings.Cut(part, "-")
		a, err := strconv.Atoi(lo)
		if err != nil {
			return nil, err
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(hi); err != nil {
				return nil, err
			}
		}
		if b < a || b-a > 1<<16 {
			return nil, fmt.Errorf("bad cpu range %q", part)
		}
		for c := a; c <= b; c++ {
			cpus = append(cpus, c)
		}
	}
	return cpus, nil
}

var cpuDirName = regexp.MustCompile(`^cpu[0-9]+$`)

// sysfsCores counts physical cores, and performance and efficiency cores on
// hybrid CPUs, from Linux's /sys (sysRoot is that directory). Hardware
// threads of one core share a core_cpus_list. Intel hybrid CPUs list their
// performance and efficiency CPUs under devices/cpu_core and
// devices/cpu_atom; ARM big.LITTLE CPUs report a cpu_capacity per CPU.
func sysfsCores(sysRoot string) (physical, perf, eff int) {
	base := filepath.Join(sysRoot, "devices", "system", "cpu")
	entries, err := os.ReadDir(base)
	if err != nil {
		return 0, 0, 0
	}
	read := func(parts ...string) string {
		b, err := os.ReadFile(filepath.Join(parts...))
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	}
	coreOf := map[int]string{} // cpu -> its core's sibling list
	var cores []string         // distinct cores, in order
	capacity := map[string]int{}
	for _, e := range entries {
		if !cpuDirName.MatchString(e.Name()) {
			continue
		}
		cpu, _ := strconv.Atoi(strings.TrimPrefix(e.Name(), "cpu"))
		dir := filepath.Join(base, e.Name())
		key := read(dir, "topology", "core_cpus_list")
		if key == "" {
			key = read(dir, "topology", "thread_siblings_list")
		}
		if key == "" {
			continue // offline, or topology not exposed
		}
		coreOf[cpu] = key
		if _, seen := capacity[key]; !seen {
			cores = append(cores, key)
			capacity[key] = -1
			if c, err := strconv.Atoi(read(dir, "cpu_capacity")); err == nil {
				capacity[key] = c
			}
		}
	}
	physical = len(cores)

	// Intel hybrid: performance CPUs are listed under cpu_core.
	if list := read(sysRoot, "devices", "cpu_core", "cpus"); list != "" && read(sysRoot, "devices", "cpu_atom", "cpus") != "" {
		if cpus, err := parseCPUList(list); err == nil {
			perfCores := map[string]bool{}
			for _, c := range cpus {
				if k, ok := coreOf[c]; ok {
					perfCores[k] = true
				}
			}
			if n := len(perfCores); n > 0 && n < physical {
				return physical, n, physical - n
			}
		}
	}
	// ARM big.LITTLE: the fastest cores have the highest capacity.
	classes := make([]int, 0, len(cores))
	for _, k := range cores {
		if capacity[k] < 0 {
			return physical, 0, 0
		}
		classes = append(classes, capacity[k])
	}
	perf, eff = splitClasses(classes)
	return physical, perf, eff
}

// sysctlSpecs builds Specs from macOS sysctl values; get returns "" for a
// name the system doesn't have.
func sysctlSpecs(get func(name string) string) Specs {
	num := func(name string) int64 {
		n, _ := strconv.ParseInt(get(name), 10, 64)
		return n
	}
	s := Specs{
		CPUModel:      get("machdep.cpu.brand_string"),
		PhysicalCores: int(num("hw.physicalcpu")),
		MemoryMB:      num("hw.memsize") / (1 << 20),
	}
	// Apple silicon numbers its core types from fastest (perflevel0) down.
	if num("hw.nperflevels") >= 2 {
		p, e := int(num("hw.perflevel0.physicalcpu")), int(num("hw.perflevel1.physicalcpu"))
		if p > 0 && e > 0 && p+e == s.PhysicalCores {
			s.PerfCores, s.EffCores = p, e
		}
	}
	return s
}
