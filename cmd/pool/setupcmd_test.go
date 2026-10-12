package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/montjeffrey/compute-pool/internal/hwinfo"
)

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
