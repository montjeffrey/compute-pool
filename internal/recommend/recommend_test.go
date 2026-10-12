package recommend

import (
	"reflect"
	"strings"
	"testing"

	"github.com/montjeffrey/compute-pool/internal/hwinfo"
	"github.com/montjeffrey/compute-pool/internal/proto"
)

// curve builds a capacity from rates relative to one job.
func curve(onBattery bool, pairs ...float64) *proto.Capacity {
	c := &proto.Capacity{Kernel: "test", OnBattery: onBattery}
	for i := 0; i+1 < len(pairs); i += 2 {
		c.Levels = append(c.Levels, proto.Level{Jobs: int(pairs[i]), Rate: 100 * pairs[i+1]})
	}
	return c
}

func setting(r Result, name string) Setting {
	for _, s := range r.Settings {
		if s.Name == name {
			return s
		}
	}
	return Setting{}
}

func TestKneeOfMeasuredCurve(t *testing.T) {
	// Throughput flattens after 8 jobs: 8 reach 92% of the peak at 16.
	in := Input{
		Specs:      hwinfo.Specs{LogicalCPUs: 32, PhysicalCores: 16, MemoryMB: 65536},
		PowerKnown: true, BaselineBusyPct: 3,
		Capacity: curve(false, 1, 1, 2, 2, 4, 3.9, 8, 7.2, 16, 7.8, 32, 7.5),
	}
	r := Recommend(in)
	if r.MaxJobs != 8 {
		t.Fatalf("MaxJobs = %d, want 8", r.MaxJobs)
	}
	if got := setting(r, "max jobs").Reason; !strings.Contains(got, "8 jobs reach 92%") || !strings.Contains(got, "7.2×") {
		t.Errorf("reason = %q", got)
	}
	if len(r.Warnings) != 0 {
		t.Errorf("warnings = %v", r.Warnings)
	}
	if setting(r, "lend on battery").Name != "" {
		t.Error("lend on battery recommended for a device without a battery")
	}
}

func TestLeavesThreadsForTheOwner(t *testing.T) {
	// Throughput keeps rising to all 16 threads, but a quarter is kept free.
	in := Input{
		Specs:      hwinfo.Specs{LogicalCPUs: 16, PhysicalCores: 8, MemoryMB: 16384},
		PowerKnown: true, HasBattery: true, BaselineBusyPct: -1,
		Capacity: curve(false, 1, 1, 2, 2, 4, 3.8, 8, 6, 16, 7),
	}
	r := Recommend(in)
	if r.MaxJobs != 12 {
		t.Fatalf("MaxJobs = %d, want 12", r.MaxJobs)
	}
	if got := setting(r, "max jobs").Reason; !strings.Contains(got, "leaves 4 threads of 16 free") {
		t.Errorf("reason = %q", got)
	}
	if s := setting(r, "lend on battery"); s.Value != "no" || r.LendOnBattery {
		t.Errorf("lend on battery = %+v, result %v", s, r.LendOnBattery)
	}
	// KeepThreads overrides the default reserve.
	in.KeepThreads = 8
	if r := Recommend(in); r.MaxJobs != 8 {
		t.Errorf("with KeepThreads 8: MaxJobs = %d, want 8", r.MaxJobs)
	}
}

func TestMemoryLimit(t *testing.T) {
	in := Input{
		Specs:      hwinfo.Specs{LogicalCPUs: 8, PhysicalCores: 8, MemoryMB: 4096},
		PowerKnown: true, BaselineBusyPct: -1,
		JobMemoryMB: 1536,
	}
	// 4096 MB keeps 1024 for the system; 3072 / 1536 = 2 jobs, fewer than
	// the 6 the thread limit allows.
	r := Recommend(in)
	if r.MaxJobs != 2 || !strings.Contains(setting(r, "max jobs").Reason, "limited by memory") {
		t.Errorf("MaxJobs = %d, reason %q", r.MaxJobs, setting(r, "max jobs").Reason)
	}
}

func TestUnmeasured(t *testing.T) {
	in := Input{Specs: hwinfo.Specs{LogicalCPUs: 8, PhysicalCores: 4}, PowerKnown: true, BaselineBusyPct: -1}
	r := Recommend(in)
	if r.MaxJobs != 4 || r.Speedup != 0 || !strings.Contains(setting(r, "max jobs").Reason, "one job per physical core") {
		t.Errorf("with cores: %+v", r)
	}
	in.Specs.PhysicalCores = 0
	if r := Recommend(in); r.MaxJobs != 4 || !strings.Contains(setting(r, "max jobs").Reason, "two hardware threads") {
		t.Errorf("without cores: MaxJobs = %d, reason %q", r.MaxJobs, setting(r, "max jobs").Reason)
	}
	in.Specs.LogicalCPUs = 1
	if r := Recommend(in); r.MaxJobs != 1 {
		t.Errorf("one CPU: MaxJobs = %d, want 1", r.MaxJobs)
	}
}

func TestWarnings(t *testing.T) {
	in := Input{
		Specs:           hwinfo.Specs{LogicalCPUs: 8, PhysicalCores: 8},
		BaselineBusyPct: 40,
		Capacity:        curve(true, 1, 1, 8, 6),
	}
	r := Recommend(in)
	want := []string{"measured on battery", "40% busy", "battery"}
	if len(r.Warnings) != 3 {
		t.Fatalf("warnings = %v", r.Warnings)
	}
	for i, w := range want {
		if !strings.Contains(r.Warnings[i], w) {
			t.Errorf("warning %d = %q, want it to mention %q", i, r.Warnings[i], w)
		}
	}
}

func TestArgs(t *testing.T) {
	r := Result{MaxJobs: 6, BusyThreshold: DefaultBusyThreshold}
	if got, want := r.Args([]string{"desktop", "laptop"}), []string{"--allow", "desktop,laptop", "--max-jobs", "6"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Args = %q, want %q", got, want)
	}
	r = Result{MaxJobs: 2, LendOnBattery: true, BusyThreshold: 60}
	if got, want := r.Args(nil), []string{"--max-jobs", "2", "--lend-on-battery", "--busy-threshold", "60"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Args = %q, want %q", got, want)
	}
}
