package main

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/montjeffrey/compute-pool/internal/advise"
	"github.com/montjeffrey/compute-pool/internal/capacity"
	"github.com/montjeffrey/compute-pool/internal/client"
	"github.com/montjeffrey/compute-pool/internal/hwinfo"
	"github.com/montjeffrey/compute-pool/internal/proto"
	"github.com/montjeffrey/compute-pool/internal/tsutil"
)

func TestPoolDevicesFillsInThisDeviceLocally(t *testing.T) {
	path := useCapacityFile(t)
	saved := proto.Capacity{Kernel: capacity.Kernel, MeasuredAt: time.Now().UTC(), Levels: []proto.Level{{Jobs: 1, Rate: 100}}}
	if err := capacity.Save(path, saved); err != nil {
		t.Fatal(err)
	}
	remote := hwinfo.Specs{CPUModel: "remote", LogicalCPUs: 4}
	results := []probeResult{
		// This device's agent is too old to report specs or a measurement.
		{peer: tsutil.Peer{Name: "desktop", Self: true}, info: proto.Info{Version: "0.1.4", MaxJobs: 10, Accepting: true}},
		{peer: tsutil.Peer{Name: "laptop"}, info: proto.Info{Version: "0.1.5", MaxJobs: 4, Accepting: false, Reason: "on battery",
			HasBattery: true, OnBattery: true, Specs: &remote}},
		{peer: tsutil.Peer{Name: "server"}, err: errors.New("connection refused")},
	}
	devs := poolDevices(results)
	if len(devs) != 3 {
		t.Fatalf("got %d devices", len(devs))
	}
	self, laptop, server := devs[0], devs[1], devs[2]
	if self.Specs == nil || self.Specs.LogicalCPUs < 1 || self.Capacity == nil || self.Capacity.RateAt(1) != 100 {
		t.Errorf("this device wasn't filled in locally: specs %+v, capacity %+v", self.Specs, self.Capacity)
	}
	if laptop.Specs != &remote || laptop.Capacity != nil || laptop.Accepting || laptop.Reason != "on battery" || !laptop.OnBattery {
		t.Errorf("laptop = %+v", laptop)
	}
	if server.Err == nil {
		t.Error("unreachable device has no error")
	}
}

func TestPrintAdvice(t *testing.T) {
	rep := advise.Report{
		Reference: "desktop",
		Rows: []advise.Row{
			{Name: "desktop", Self: true, CPU: "i7-12700KF, 12c/20t", Lends: 10, Status: "ready", Capacity: 8, Share: 0.72,
				Advice: []string{"lends 10 jobs; pool setup recommends 15 (+9% pool capacity)"}},
			{Name: "laptop", CPU: "Ryzen 7 7730U, 8c/16t", Lends: 12, Status: "paused: on battery", Capacity: 3.1, Share: 0.28},
			{Name: "nas", Status: "no agent"},
		},
		Total: 11.1, Available: 8, SelfCapacity: 8, Speedup: 1.38, RecommendedTotal: 12.1, RecommendedSpeedup: 1.34,
	}
	var buf bytes.Buffer
	printAdvice(&buf, rep)
	out := buf.String()
	for _, w := range []string{
		"desktop (this device)  i7-12700KF, 12c/20t",
		"units of one job on desktop",
		"up to 1.38× as fast",
		"leaving 8.0 (72%) available",
		"capacity 12.1, up to 1.34×",
		"desktop: lends 10 jobs; pool setup recommends 15",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
	for _, re := range []string{`(?m)^desktop \(this device\).*\b10\s+8\.0\s+72%\s+ready$`, `(?m)^nas\s+-\s+-\s+-\s+-\s+no agent$`} {
		if !regexp.MustCompile(re).MatchString(out) {
			t.Errorf("output doesn't match %s:\n%s", re, out)
		}
	}

	buf.Reset()
	printAdvice(&buf, advise.Report{Rows: []advise.Row{{Name: "desktop", Self: true, Status: "ready", Lends: 4}}})
	if !strings.Contains(buf.String(), "No device has been measured yet") {
		t.Errorf("unmeasured pool:\n%s", buf.String())
	}
}

func TestProbeStatus(t *testing.T) {
	busy := fmt.Errorf("%w: can't confirm who is asking yet; try again shortly", client.ErrBusy)
	for _, c := range []struct {
		err  error
		want string
	}{
		{client.ErrForbidden, "agent refuses this device (add it to --allow)"},
		{busy, "agent busy: can't confirm who is asking yet; try again shortly"},
		{errors.New("connection refused"), "no agent"},
	} {
		if got := probeStatus(c.err); got != c.want {
			t.Errorf("probeStatus(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}
