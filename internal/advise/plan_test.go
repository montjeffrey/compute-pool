package advise

import (
	"strings"
	"testing"
	"time"
)

// self runs one job at 100 units/s and two at 200; remote at half that.
func planDevs() []PlanDevice {
	return []PlanDevice{
		{Name: "self", Self: true, Slots: 2, Capacity: capOf(1, 100, 2, 200)},
		{Name: "remote", Slots: 2, Capacity: capOf(1, 50, 2, 100)},
	}
}

func warned(p Plan, s string) bool {
	for _, w := range p.Warnings {
		if strings.Contains(w, s) {
			return true
		}
	}
	return false
}

func TestPlanSplitsWorkBySpeed(t *testing.T) {
	// A task takes 1s alone here: 1.1s with overhead locally, 2.15s on the
	// half-speed remote. The six tasks end at 2.2s (plus 1s to start) using
	// both, against three rounds of 1.1s (plus 1s) here alone.
	p := PlanJob(planDevs(), 6, time.Second, 0)
	if p.Pool != 3200*time.Millisecond || p.Alone != 4300*time.Millisecond {
		t.Fatalf("pool %v, alone %v", p.Pool, p.Alone)
	}
	if p.Speedup < 1.34 || p.Speedup > 1.35 {
		t.Errorf("speedup = %v", p.Speedup)
	}
	self, remote := p.Rows[0], p.Rows[1]
	if self.Tasks != 4 || remote.Tasks != 2 || self.TaskTime != 1100*time.Millisecond || remote.TaskTime != 2150*time.Millisecond {
		t.Errorf("rows: %+v, %+v", self, remote)
	}
	if len(p.Warnings) != 0 {
		t.Errorf("warnings: %v", p.Warnings)
	}
}

func TestPlanWarnsAboutShortAndFewTasks(t *testing.T) {
	p := PlanJob(planDevs(), 2, 200*time.Millisecond, 0)
	for _, w := range []string{"are short", "2 tasks for 4 free slots", "barely beats this device"} {
		if !warned(p, w) {
			t.Errorf("no warning about %q: %v", w, p.Warnings)
		}
	}
}

func TestPlanSkipsPausedDevices(t *testing.T) {
	devs := planDevs()
	devs[1].Paused = true
	p := PlanJob(devs, 6, time.Second, 0)
	if p.Rows[1].Tasks != 0 || !p.Rows[1].Paused || p.Rows[0].Tasks != 6 || p.Pool != p.Alone {
		t.Errorf("plan = %+v", p)
	}
}

func TestPlanWithoutTaskTime(t *testing.T) {
	p := PlanJob(planDevs(), 6, 0, 0)
	if p.Pool != 0 || p.Alone != 0 || p.Speedup != 0 || p.Rows[0].TaskTime != 0 {
		t.Errorf("times predicted without a task time: %+v", p)
	}
	if p.Rows[0].Tasks != 4 || p.Rows[1].Tasks != 2 || p.Rows[0].Share < 0.66 || p.Rows[0].Share > 0.67 {
		t.Errorf("split: %+v", p.Rows)
	}
}

func TestPlanUnmeasuredAndCapped(t *testing.T) {
	devs := planDevs()
	devs[1].Capacity = nil
	p := PlanJob(devs, 4, time.Second, 0)
	if !p.Rows[1].Assumed || p.Rows[1].TaskTime != 1150*time.Millisecond {
		t.Errorf("unmeasured device: %+v", p.Rows[1])
	}
	// --jobs 2 leaves the slower slots idle.
	p = PlanJob(planDevs(), 4, time.Second, 2)
	if p.Rows[0].Tasks != 4 || p.Rows[1].Tasks != 0 {
		t.Errorf("capped: %+v", p.Rows)
	}
}
