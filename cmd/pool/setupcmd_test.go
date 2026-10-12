package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/montjeffrey/compute-pool/internal/capacity"
	"github.com/montjeffrey/compute-pool/internal/hwinfo"
	"github.com/montjeffrey/compute-pool/internal/proto"
	"github.com/montjeffrey/compute-pool/internal/recommend"
)

// useCapacityFile points pool setup at a test file for the rest of the test.
func useCapacityFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "capacity.json")
	old := capacityPath
	capacityPath = func() string { return path }
	t.Cleanup(func() { capacityPath = old })
	return path
}

func TestSetupJSONUsesSavedMeasurement(t *testing.T) {
	path := useCapacityFile(t)
	saved := proto.Capacity{Kernel: capacity.Kernel, MeasuredAt: time.Now().UTC(),
		Levels: []proto.Level{{Jobs: 1, Rate: 100}, {Jobs: 2, Rate: 200}}}
	if err := capacity.Save(path, saved); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	err := cmdSetup(context.Background(), []string{"--no-measure", "--json", "--allow", "desktop, laptop"}, strings.NewReader(""), &out, &errb)
	if err != nil {
		t.Fatalf("setup: %v (%s)", err, errb.String())
	}
	var rep setupReport
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out.String())
	}
	if rep.Capacity == nil || len(rep.Capacity.Levels) != 2 {
		t.Errorf("capacity = %+v", rep.Capacity)
	}
	if rep.Recommendation.MaxJobs < 1 || len(rep.Recommendation.Settings) == 0 {
		t.Errorf("recommendation = %+v", rep.Recommendation)
	}
	want := []string{"pool", "install", "--allow", "desktop,laptop", "--max-jobs"}
	if len(rep.Command) < len(want) || !reflect.DeepEqual(rep.Command[:len(want)], want) {
		t.Errorf("command = %q", rep.Command)
	}
}

func TestSetupWithoutMeasurementDeclined(t *testing.T) {
	useCapacityFile(t) // empty: nothing saved
	var out, errb bytes.Buffer
	err := cmdSetup(context.Background(), []string{"--no-measure", "--allow", "desktop"}, strings.NewReader("n\n"), &out, &errb)
	if err != nil {
		t.Fatalf("setup: %v (%s)", err, errb.String())
	}
	got := out.String()
	// The max jobs reason depends on this machine's CPU count, so only the
	// setting itself is checked.
	for _, w := range []string{"No saved measurement", "Recommended settings:", "  max jobs ", "allowed devices  desktop"} {
		if !strings.Contains(got, w) {
			t.Errorf("output lacks %q:\n%s", w, got)
		}
	}
	// Declining (or, off Windows, never being asked) must change nothing.
	if runtime.GOOS == "windows" {
		if !strings.Contains(got, "Nothing changed.") || strings.Contains(got, "Installed") {
			t.Errorf("declining didn't leave things alone:\n%s", got)
		}
	} else if !strings.Contains(got, "pool agent --allow desktop --max-jobs") {
		t.Errorf("no agent command shown:\n%s", got)
	}
}

func TestPrintRecommendation(t *testing.T) {
	r := recommend.Result{MaxJobs: 6, Settings: []recommend.Setting{{Name: "max jobs", Value: "6", Reason: "because"}},
		Warnings: []string{"measured on battery"}}
	var buf bytes.Buffer
	printRecommendation(&buf, r, []string{"desktop", "phone"}, "from --allow")
	for _, w := range []string{"  max jobs         6 ", " because\n", "allowed devices  desktop, phone  from --allow", "Note: measured on battery."} {
		if !strings.Contains(buf.String(), w) {
			t.Errorf("output lacks %q:\n%s", w, buf.String())
		}
	}
}

func TestPrintDevice(t *testing.T) {
	cases := []struct {
		name string
		d    deviceReport
		want []string
	}{
		{
			name: "hybrid laptop on battery",
			d: deviceReport{Name: "laptop", OS: "windows", Arch: "amd64", PowerKnown: true, HasBattery: true, OnBattery: true, BatteryPct: 40,
				Specs: hwinfo.Specs{CPUModel: "Example CPU", LogicalCPUs: 16, PhysicalCores: 12, PerfCores: 4, EffCores: 8, MemoryMB: 16384}},
			want: []string{
				"This device: laptop (windows/amd64)",
				"CPU     Example CPU",
				"Cores   12 cores (4 performance + 8 efficiency), 16 threads",
				"Memory  16.0 GB",
				"Power   laptop, on battery, 40%",
			},
		},
		{
			name: "desktop",
			d: deviceReport{Name: "desktop", OS: "linux", Arch: "arm64", PowerKnown: true, BatteryPct: -1,
				Specs: hwinfo.Specs{CPUModel: "Example CPU", LogicalCPUs: 8, PhysicalCores: 8, MemoryMB: 8000}},
			want: []string{"Cores   8 cores, 8 threads", "Memory  7.8 GB", "Power   no battery (desktop or server)"},
		},
		{
			name: "nothing known",
			d:    deviceReport{Name: "box", OS: "plan9", Arch: "386", BatteryPct: -1, Specs: hwinfo.Specs{LogicalCPUs: 1}},
			want: []string{"CPU     unknown", "Cores   unknown, 1 thread", "Memory  unknown", "Power   unknown"},
		},
	}
	for _, c := range cases {
		var buf bytes.Buffer
		printDevice(&buf, c.d)
		for _, w := range c.want {
			if !strings.Contains(buf.String(), w) {
				t.Errorf("%s: output lacks %q:\n%s", c.name, w, buf.String())
			}
		}
	}
}
