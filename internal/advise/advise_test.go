package advise

import (
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/montjeffrey/compute-pool/internal/hwinfo"
	"github.com/montjeffrey/compute-pool/internal/proto"
)

func capOf(pairs ...float64) *proto.Capacity {
	c := &proto.Capacity{Kernel: "k1"}
	for i := 0; i+1 < len(pairs); i += 2 {
		c.Levels = append(c.Levels, proto.Level{Jobs: int(pairs[i]), Rate: pairs[i+1]})
	}
	return c
}

func near(a, b float64) bool { return math.Abs(a-b) < 0.01 }

// desktop is this device: one job runs at 100 units/s, 10 at 800, 20 at 1000.
func desktop() Device {
	return Device{Name: "desktop", Self: true, MaxJobs: 10, Accepting: true, PowerKnown: true,
		Specs:    &hwinfo.Specs{CPUModel: "12th Gen Intel(R) Core(TM) i7-12700KF", LogicalCPUs: 20, PhysicalCores: 12, PerfCores: 8, EffCores: 4},
		Capacity: capOf(1, 100, 10, 800, 20, 1000)}
}

// laptop: one job at 50, 16 at 400; lending 12 gives 50+11/15*350 ≈ 306.7.
func laptop() Device {
	return Device{Name: "laptop", MaxJobs: 12, Accepting: true, PowerKnown: true, HasBattery: true,
		Specs:    &hwinfo.Specs{CPUModel: "AMD Ryzen 7 7730U with Radeon Graphics", LogicalCPUs: 16, PhysicalCores: 8},
		Capacity: capOf(1, 50, 16, 400)}
}

func row(rep Report, name string) Row {
	for _, r := range rep.Rows {
		if r.Name == name {
			return r
		}
	}
	return Row{}
}

func adviceContains(r Row, s string) bool {
	for _, a := range r.Advice {
		if strings.Contains(a, s) {
			return true
		}
	}
	return false
}

func TestTwoMeasuredDevices(t *testing.T) {
	rep := Analyze([]Device{desktop(), laptop()})
	if rep.Reference != "desktop" {
		t.Errorf("reference = %q", rep.Reference)
	}
	d, l := row(rep, "desktop"), row(rep, "laptop")
	if !near(d.Capacity, 8) || !near(l.Capacity, 3.0667) || !near(rep.Total, 11.0667) || !near(rep.Available, rep.Total) {
		t.Errorf("capacities: desktop %v, laptop %v, total %v, available %v", d.Capacity, l.Capacity, rep.Total, rep.Available)
	}
	if !near(rep.SelfCapacity, 8) || !near(rep.Speedup, 1.3833) {
		t.Errorf("self %v, speedup %v", rep.SelfCapacity, rep.Speedup)
	}
	if !near(d.Share, 0.7229) || !near(l.Share, 0.2771) {
		t.Errorf("shares: %v, %v", d.Share, l.Share)
	}
	// The desktop's throughput keeps rising to 20 jobs, so pool setup would
	// lend 15 (keeping 5 threads); at 15 it does 900, i.e. 9.0.
	if d.Recommended != 15 || !adviceContains(d, "lends 10 jobs; pool setup recommends 15 (+9% pool capacity)") {
		t.Errorf("desktop: recommended %d, advice %q", d.Recommended, d.Advice)
	}
	if l.Recommended != 12 || len(l.Advice) != 0 {
		t.Errorf("laptop: recommended %d, advice %q", l.Recommended, l.Advice)
	}
	if !near(rep.RecommendedTotal, 12.0667) || !near(rep.RecommendedSpeedup, 1.3407) {
		t.Errorf("recommended: total %v, speedup %v", rep.RecommendedTotal, rep.RecommendedSpeedup)
	}
	if d.CPU != "i7-12700KF, 12c/20t" || l.CPU != "AMD Ryzen 7 7730U, 8c/16t" {
		t.Errorf("cpu summaries: %q, %q", d.CPU, l.CPU)
	}
}

func TestPausedAndUnusableDevices(t *testing.T) {
	onBattery := laptop()
	onBattery.Accepting, onBattery.Reason, onBattery.OnBattery = false, "on battery", true
	busy := laptop()
	busy.Name, busy.Accepting, busy.Reason = "busy", false, "all 12 job slots in use"
	old := Device{Name: "old", MaxJobs: 4, Accepting: true, Version: "0.1.4"}
	gone := Device{Name: "gone", Err: errors.New("no agent")}
	rep := Analyze([]Device{desktop(), onBattery, busy, old, gone})

	// Paused on battery: counted in Total but not Available. A device whose
	// slots are all busy is working, not paused.
	if !near(rep.Total, 8+3.0667*2) || !near(rep.Available, 8+3.0667) {
		t.Errorf("total %v, available %v", rep.Total, rep.Available)
	}
	if r := row(rep, "laptop"); r.Status != "paused: on battery" || !adviceContains(r, "paused now") {
		t.Errorf("laptop: %+v", r)
	}
	if r := row(rep, "busy"); adviceContains(r, "paused now") {
		t.Errorf("busy device called paused: %q", r.Advice)
	}
	if r := row(rep, "old"); r.Capacity != 0 || !adviceContains(r, "runs pool 0.1.4") {
		t.Errorf("old agent: %+v", r)
	}
	if len(rep.Unmeasured) != 1 || rep.Unmeasured[0] != "old" {
		t.Errorf("unmeasured = %v", rep.Unmeasured)
	}
	if r := row(rep, "gone"); r.Status != "no agent" || r.Capacity != 0 || len(r.Advice) != 0 {
		t.Errorf("unreachable: %+v", r)
	}
}

func TestSmallDeviceAndOtherKernel(t *testing.T) {
	tiny := Device{Name: "tiny", MaxJobs: 1, Accepting: true, PowerKnown: true,
		Specs: &hwinfo.Specs{LogicalCPUs: 4, PhysicalCores: 4}, Capacity: capOf(1, 40, 4, 150)}
	other := laptop()
	other.Name, other.Capacity = "other", &proto.Capacity{Kernel: "k0", Levels: []proto.Level{{Jobs: 1, Rate: 1}}}
	rep := Analyze([]Device{desktop(), tiny, other})
	if r := row(rep, "tiny"); !adviceContains(r, "adds only 5%") {
		t.Errorf("tiny: %+v", r)
	}
	if r := row(rep, "other"); r.Capacity != 0 || !adviceContains(r, "different pool version") {
		t.Errorf("other kernel: %+v", r)
	}
}

func TestSelfUnmeasured(t *testing.T) {
	self := desktop()
	self.Capacity = nil
	rep := Analyze([]Device{self, laptop()})
	if rep.Reference != "laptop" || rep.SelfCapacity != 0 || rep.Speedup != 0 {
		t.Errorf("reference %q, self %v, speedup %v", rep.Reference, rep.SelfCapacity, rep.Speedup)
	}
	// In units of one laptop job: 306.7 / 50.
	if r := row(rep, "laptop"); !near(r.Capacity, 6.1333) {
		t.Errorf("laptop capacity = %v", r.Capacity)
	}
	if r := row(rep, "desktop"); !adviceContains(r, "not measured yet") {
		t.Errorf("desktop advice = %q", r.Advice)
	}

	// Nothing measured at all.
	rep = Analyze([]Device{self})
	if rep.Reference != "" || rep.Total != 0 || len(rep.Unmeasured) != 1 {
		t.Errorf("nothing measured: %+v", rep)
	}
}
