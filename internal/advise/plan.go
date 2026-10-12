package advise

import (
	"fmt"
	"sort"
	"time"

	"github.com/montjeffrey/compute-pool/internal/proto"
)

// Fixed costs measured on real devices: starting pool map (asking Tailscale
// and every agent for its status) and each task's round trip and launch.
const (
	StartupCost    = time.Second
	LocalTaskCost  = 100 * time.Millisecond
	RemoteTaskCost = 150 * time.Millisecond
)

// shortTask is the task length below which the fixed costs above take a
// noticeable share of the time.
const shortTask = time.Second

// PlanDevice is a device pool map would use.
type PlanDevice struct {
	Name     string
	Self     bool
	Slots    int
	Paused   bool
	Capacity *proto.Capacity
}

// PlanRow is one device in a plan.
type PlanRow struct {
	Name  string `json:"name"`
	Self  bool   `json:"self,omitempty"`
	Slots int    `json:"slots"`
	// TaskTime is one task's predicted duration on this device with all
	// its slots busy, overhead included; 0 without a task time.
	TaskTime time.Duration `json:"task_time,omitempty"`
	Share    float64       `json:"share"` // of the tasks, predicted
	Tasks    int           `json:"tasks,omitempty"`
	Assumed  bool          `json:"assumed,omitempty"` // not measured; as fast as this device assumed
	Paused   bool          `json:"paused,omitempty"`
}

// Plan predicts how a pool map run would go.
type Plan struct {
	Tasks    int           `json:"tasks"`
	TaskTime time.Duration `json:"task_time,omitempty"`
	Rows     []PlanRow     `json:"devices"`
	// Pool and Alone are predicted run times using every usable device and
	// this device alone (0 when unknown).
	Pool     time.Duration `json:"pool,omitempty"`
	Alone    time.Duration `json:"alone,omitempty"`
	Speedup  float64       `json:"speedup,omitempty"`
	Warnings []string      `json:"warnings,omitempty"`
}

// PlanJob predicts a run of tasks over devs. taskTime is how long one task
// takes alone on this device (0 if unknown); maxParallel caps tasks running
// at once across all devices (0 = no cap).
func PlanJob(devs []PlanDevice, tasks int, taskTime time.Duration, maxParallel int) Plan {
	p := Plan{Tasks: tasks, TaskTime: taskTime}

	// The unit is one job on this device, or on the first measured device.
	var unit float64
	for _, d := range devs {
		if r := d.Capacity.RateAt(1); r > 0 && (d.Self || unit == 0) {
			unit = r
			if d.Self {
				break
			}
		}
	}

	type slot struct {
		row  int
		cost time.Duration // per task
		free time.Duration
	}
	var poolSlots, selfSlots []slot
	totalSlots := 0
	for _, d := range devs {
		row := PlanRow{Name: d.Name, Self: d.Self, Slots: d.Slots, Paused: d.Paused}
		// Per-job speed relative to the unit, with all slots busy.
		perJob := 1.0
		if r := d.Capacity.RateAt(d.Slots); r > 0 && unit > 0 && d.Slots > 0 {
			perJob = r / float64(d.Slots) / unit
		} else {
			row.Assumed = true
		}
		overhead := RemoteTaskCost
		if d.Self {
			overhead = LocalTaskCost
		}
		var cost time.Duration
		if taskTime > 0 {
			cost = overhead + time.Duration(float64(taskTime)/perJob)
			row.TaskTime = cost
		} else {
			// Without a task time, a nominal one still gives the split.
			cost = time.Duration(float64(time.Second) / perJob)
		}
		p.Rows = append(p.Rows, row)
		if d.Paused {
			continue
		}
		totalSlots += d.Slots
		for range d.Slots {
			s := slot{row: len(p.Rows) - 1, cost: cost}
			poolSlots = append(poolSlots, s)
			if d.Self {
				selfSlots = append(selfSlots, s)
			}
		}
	}
	if maxParallel > 0 && maxParallel < len(poolSlots) {
		// A cap on parallel tasks leaves the slowest slots idle.
		sort.SliceStable(poolSlots, func(i, j int) bool { return poolSlots[i].cost < poolSlots[j].cost })
		poolSlots = poolSlots[:maxParallel]
	}

	// simulate hands each task to the slot that would finish it first. That
	// is pool map's behavior once its backup copies of slow final tasks are
	// counted: a slow device's last task is redone on a faster one.
	simulate := func(slots []slot, count bool) time.Duration {
		if len(slots) == 0 || tasks == 0 {
			return 0
		}
		s := append([]slot(nil), slots...)
		for range tasks {
			best := 0
			for i := range s {
				if s[i].free+s[i].cost < s[best].free+s[best].cost {
					best = i
				}
			}
			s[best].free += s[best].cost
			if count {
				p.Rows[s[best].row].Tasks++
			}
		}
		var end time.Duration
		for _, x := range s {
			end = max(end, x.free)
		}
		return end + StartupCost
	}
	pool := simulate(poolSlots, true)
	alone := simulate(selfSlots, false)
	for i := range p.Rows {
		if tasks > 0 {
			p.Rows[i].Share = float64(p.Rows[i].Tasks) / float64(tasks)
		}
	}
	if taskTime > 0 {
		p.Pool, p.Alone = pool, alone
		if alone > 0 && pool > 0 {
			p.Speedup = float64(alone) / float64(pool)
		}
	}

	if taskTime > 0 && taskTime < shortTask {
		p.Warnings = append(p.Warnings, fmt.Sprintf(
			"tasks of %s are short: each also costs about %s to send and start, so pooling gains little; give each task several items",
			taskTime, RemoteTaskCost))
	}
	if tasks < totalSlots {
		p.Warnings = append(p.Warnings, fmt.Sprintf(
			"%d tasks for %d free slots: some slots stay idle, and the slowest device decides when the run ends", tasks, totalSlots))
	}
	if p.Speedup > 0 && p.Speedup < 1.05 {
		p.Warnings = append(p.Warnings, "the pool barely beats this device alone for this job; run it here with --nodes and this device's name")
	}
	return p
}
