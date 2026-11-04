package hwinfo

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
)

// coreRecord builds a Windows RelationProcessorCore record with one
// GROUP_AFFINITY whose mask has the given number of threads.
func coreRecord(ptrSize int, class byte, threads int) []byte {
	size := 32 + ptrSize + 8
	rec := make([]byte, size)
	binary.LittleEndian.PutUint32(rec[0:], relationProcessorCore)
	binary.LittleEndian.PutUint32(rec[4:], uint32(size))
	if threads > 1 {
		rec[8] = 1 // LTP_PC_SMT
	}
	rec[9] = class
	binary.LittleEndian.PutUint16(rec[30:], 1)
	mask := uint64(1)<<threads - 1
	if ptrSize == 8 {
		binary.LittleEndian.PutUint64(rec[32:], mask)
	} else {
		binary.LittleEndian.PutUint32(rec[32:], uint32(mask))
	}
	return rec
}

func otherRecord(rel uint32) []byte {
	rec := make([]byte, 32)
	binary.LittleEndian.PutUint32(rec[0:], rel)
	binary.LittleEndian.PutUint32(rec[4:], 32)
	return rec
}

func TestParseProcessorCores(t *testing.T) {
	for _, ptrSize := range []int{8, 4} {
		// 2 performance cores with 2 threads each, 3 efficiency cores, and a
		// cache record that must be skipped.
		var buf []byte
		buf = append(buf, coreRecord(ptrSize, 1, 2)...)
		buf = append(buf, otherRecord(2)...)
		buf = append(buf, coreRecord(ptrSize, 1, 2)...)
		for i := 0; i < 3; i++ {
			buf = append(buf, coreRecord(ptrSize, 0, 1)...)
		}
		classes, err := parseProcessorCores(buf, ptrSize)
		if err != nil {
			t.Fatalf("ptrSize %d: %v", ptrSize, err)
		}
		if want := []int{1, 1, 0, 0, 0}; !reflect.DeepEqual(classes, want) {
			t.Errorf("ptrSize %d: classes = %v, want %v", ptrSize, classes, want)
		}
		if p, e := splitClasses(classes); p != 2 || e != 3 {
			t.Errorf("ptrSize %d: split = %d perf, %d eff; want 2, 3", ptrSize, p, e)
		}
	}
}

func TestParseProcessorCoresRejectsBadInput(t *testing.T) {
	good := coreRecord(8, 0, 2)
	for name, buf := range map[string][]byte{
		"truncated header": good[:5],
		"size past end":    good[:len(good)-1],
		"zero size":        make([]byte, 16),
	} {
		if _, err := parseProcessorCores(buf, 8); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestSplitClasses(t *testing.T) {
	cases := []struct {
		classes   []int
		perf, eff int
	}{
		{nil, 0, 0},
		{[]int{0, 0, 0, 0}, 0, 0}, // not hybrid
		{[]int{1, 1, 0, 0, 0, 0}, 2, 4},
		{[]int{1024, 1024, 446, 446, 446, 446}, 2, 4}, // ARM capacities
		{[]int{2, 1, 0}, 1, 2},                        // three tiers: fastest vs the rest
	}
	for _, c := range cases {
		if p, e := splitClasses(c.classes); p != c.perf || e != c.eff {
			t.Errorf("splitClasses(%v) = %d, %d; want %d, %d", c.classes, p, e, c.perf, c.eff)
		}
	}
}

func TestCPUInfoModel(t *testing.T) {
	x86 := "processor\t: 0\nvendor_id\t: GenuineIntel\nmodel name\t: Intel(R) Core(TM) i7-1260P\n\nprocessor\t: 1\nmodel name\t: Intel(R) Core(TM) i7-1260P\n"
	pi := "processor\t: 0\nBogoMIPS\t: 108.00\n\nHardware\t: BCM2835\nModel\t\t: Raspberry Pi 4 Model B Rev 1.4\n"
	none := "processor\t: 0\nBogoMIPS\t: 50.00\n"
	for in, want := range map[string]string{x86: "Intel(R) Core(TM) i7-1260P", pi: "BCM2835", none: ""} {
		if got := cpuInfoModel(in); got != want {
			t.Errorf("cpuInfoModel = %q, want %q", got, want)
		}
	}
}

func TestMemInfoMB(t *testing.T) {
	in := "MemTotal:       16303448 kB\nMemFree:         1234567 kB\n"
	if got := memInfoMB(in); got != 15921 {
		t.Errorf("memInfoMB = %d, want 15921", got)
	}
	if got := memInfoMB("MemFree: 1 kB\n"); got != 0 {
		t.Errorf("memInfoMB without MemTotal = %d, want 0", got)
	}
}

func TestParseCPUList(t *testing.T) {
	got, err := parseCPUList("0-3,8,10-11\n")
	if err != nil || !reflect.DeepEqual(got, []int{0, 1, 2, 3, 8, 10, 11}) {
		t.Errorf("parseCPUList = %v, %v", got, err)
	}
	for _, bad := range []string{"a", "3-1", "0-x"} {
		if _, err := parseCPUList(bad); err == nil {
			t.Errorf("parseCPUList(%q): no error", bad)
		}
	}
}

// fakeSys builds a /sys tree. siblings maps each CPU to its core's sibling
// list; capacity (optional) maps CPUs to cpu_capacity.
func fakeSys(t *testing.T, siblings map[int]string, capacity map[int]string, extra map[string]string) string {
	t.Helper()
	root := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for cpu, sib := range siblings {
		dir := "devices/system/cpu/cpu" + strconv.Itoa(cpu)
		write(dir+"/topology/core_cpus_list", sib)
		if c, ok := capacity[cpu]; ok {
			write(dir+"/cpu_capacity", c)
		}
	}
	write("devices/system/cpu/online", "0-63") // not a cpuN directory
	for rel, content := range extra {
		write(rel, content)
	}
	return root
}

func TestSysfsCoresIntelHybrid(t *testing.T) {
	// cpu0-3: two performance cores with two threads each; cpu4-7: four
	// efficiency cores.
	sib := map[int]string{0: "0-1", 1: "0-1", 2: "2-3", 3: "2-3", 4: "4", 5: "5", 6: "6", 7: "7"}
	root := fakeSys(t, sib, nil, map[string]string{
		"devices/cpu_core/cpus": "0-3",
		"devices/cpu_atom/cpus": "4-7",
	})
	if p, perf, eff := sysfsCores(root); p != 6 || perf != 2 || eff != 4 {
		t.Errorf("sysfsCores = %d physical, %d perf, %d eff; want 6, 2, 4", p, perf, eff)
	}
}

func TestSysfsCoresBigLittle(t *testing.T) {
	sib := map[int]string{0: "0", 1: "1", 2: "2", 3: "3", 4: "4", 5: "5"}
	capacity := map[int]string{0: "446", 1: "446", 2: "446", 3: "446", 4: "1024", 5: "1024"}
	root := fakeSys(t, sib, capacity, nil)
	if p, perf, eff := sysfsCores(root); p != 6 || perf != 2 || eff != 4 {
		t.Errorf("sysfsCores = %d physical, %d perf, %d eff; want 6, 2, 4", p, perf, eff)
	}
}

func TestSysfsCoresUniform(t *testing.T) {
	sib := map[int]string{0: "0,4", 1: "1,5", 2: "2,6", 3: "3,7", 4: "0,4", 5: "1,5", 6: "2,6", 7: "3,7"}
	root := fakeSys(t, sib, nil, nil)
	if p, perf, eff := sysfsCores(root); p != 4 || perf != 0 || eff != 0 {
		t.Errorf("sysfsCores = %d physical, %d perf, %d eff; want 4, 0, 0", p, perf, eff)
	}
	if p, _, _ := sysfsCores(filepath.Join(root, "missing")); p != 0 {
		t.Errorf("missing /sys: physical = %d, want 0", p)
	}
}

func TestSysctlSpecs(t *testing.T) {
	apple := map[string]string{
		"machdep.cpu.brand_string":  "Apple M2 Pro",
		"hw.physicalcpu":            "10",
		"hw.memsize":                "17179869184",
		"hw.nperflevels":            "2",
		"hw.perflevel0.physicalcpu": "6",
		"hw.perflevel1.physicalcpu": "4",
	}
	got := sysctlSpecs(func(n string) string { return apple[n] })
	want := Specs{CPUModel: "Apple M2 Pro", PhysicalCores: 10, PerfCores: 6, EffCores: 4, MemoryMB: 16384}
	if got != want {
		t.Errorf("Apple silicon: %+v, want %+v", got, want)
	}
	intel := map[string]string{
		"machdep.cpu.brand_string": "Intel(R) Core(TM) i7-9750H CPU @ 2.60GHz",
		"hw.physicalcpu":           "6",
		"hw.memsize":               "17179869184",
	}
	got = sysctlSpecs(func(n string) string { return intel[n] })
	if got.PhysicalCores != 6 || got.Hybrid() || got.MemoryMB != 16384 {
		t.Errorf("Intel Mac: %+v", got)
	}
}

func TestDetectOnThisMachine(t *testing.T) {
	s := Detect()
	t.Logf("%+v", s)
	if s.LogicalCPUs < 1 {
		t.Errorf("LogicalCPUs = %d", s.LogicalCPUs)
	}
	if s.PhysicalCores > s.LogicalCPUs {
		t.Errorf("%d physical cores but %d logical CPUs", s.PhysicalCores, s.LogicalCPUs)
	}
	if s.Hybrid() && s.PerfCores+s.EffCores != s.PhysicalCores {
		t.Errorf("perf %d + eff %d != physical %d", s.PerfCores, s.EffCores, s.PhysicalCores)
	}
	if s.MemoryMB < 0 {
		t.Errorf("MemoryMB = %d", s.MemoryMB)
	}
}
