package main

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/montjeffrey/compute-pool/internal/advise"
	"github.com/montjeffrey/compute-pool/internal/capacity"
	"github.com/montjeffrey/compute-pool/internal/fanout"
	"github.com/montjeffrey/compute-pool/internal/proto"
)

func TestPlanDevices(t *testing.T) {
	path := useCapacityFile(t)
	saved := proto.Capacity{Kernel: capacity.Kernel, MeasuredAt: time.Now().UTC(), Levels: []proto.Level{{Jobs: 1, Rate: 100}}}
	if err := capacity.Save(path, saved); err != nil {
		t.Fatal(err)
	}
	remote := &proto.Capacity{Kernel: capacity.Kernel, Levels: []proto.Level{{Jobs: 1, Rate: 50}}}
	devs := planDevices([]mapDevice{
		{Device: fanout.Device{Name: "desktop", Slots: 10}, self: true, info: proto.Info{Accepting: true}},
		{Device: fanout.Device{Name: "laptop", Slots: 4}, info: proto.Info{Reason: "on battery", Capacity: remote}},
		{Device: fanout.Device{Name: "server", Slots: 8}, info: proto.Info{Reason: "all 8 job slots in use"}},
	})
	if d := devs[0]; !d.Self || d.Capacity == nil || d.Capacity.RateAt(1) != 100 || d.Paused {
		t.Errorf("this device: %+v", d)
	}
	if d := devs[1]; d.Capacity != remote || !d.Paused {
		t.Errorf("laptop on battery: %+v", d)
	}
	if d := devs[2]; d.Paused || d.Capacity != nil {
		t.Errorf("busy server should count as available: %+v", d)
	}
}

func TestPrintPlan(t *testing.T) {
	p := advise.Plan{
		Tasks: 160, TaskTime: 1700 * time.Millisecond,
		Rows: []advise.PlanRow{
			{Name: "desktop", Self: true, Slots: 15, TaskTime: 2310 * time.Millisecond, Tasks: 113, Share: 0.706},
			{Name: "laptop", Slots: 12, TaskTime: 4140 * time.Millisecond, Tasks: 47, Share: 0.294},
			{Name: "nas", Slots: 4, Assumed: true, Paused: true},
		},
		Pool: 26 * time.Second, Alone: 36 * time.Second, Speedup: 1.38,
		Warnings: []string{"tasks of 200ms are short"},
	}
	var buf bytes.Buffer
	printPlan(&buf, p)
	out := buf.String()
	for _, re := range []string{
		`(?m)^Dry run: 160 tasks; nothing was run\.$`,
		`(?m)^desktop \(this device\)\s+15\s+2\.3s\s+113\s+71%$`,
		`(?m)^laptop\s+12\s+4\.1s\s+47\s+29%$`,
		`(?m)^nas \*\s+4\s+-\s+paused\s+-$`,
		`(?m)^\* not measured`,
		`about 26s using these devices, against 36s on this device alone \(1\.38× as fast\)`,
		`(?m)^Note: tasks of 200ms are short\.$`,
	} {
		if !regexp.MustCompile(re).MatchString(out) {
			t.Errorf("output doesn't match %s:\n%s", re, out)
		}
	}

	buf.Reset()
	printPlan(&buf, advise.Plan{Tasks: 3, Rows: []advise.PlanRow{{Name: "desktop", Self: true, Slots: 2, Tasks: 3, Share: 1}}})
	if !strings.Contains(buf.String(), "Add --task-time") {
		t.Errorf("no hint about --task-time:\n%s", buf.String())
	}
}
