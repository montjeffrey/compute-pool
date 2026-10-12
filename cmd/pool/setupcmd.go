package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/montjeffrey/compute-pool/internal/hwinfo"
	"github.com/montjeffrey/compute-pool/internal/sysinfo"
	"github.com/montjeffrey/compute-pool/internal/tsutil"
)

// deviceReport is what pool setup learns about this device.
type deviceReport struct {
	Name       string       `json:"name"`
	OS         string       `json:"os"`
	Arch       string       `json:"arch"`
	Specs      hwinfo.Specs `json:"specs"`
	PowerKnown bool         `json:"power_known"`
	HasBattery bool         `json:"has_battery"`
	OnBattery  bool         `json:"on_battery"`
	BatteryPct int          `json:"battery_pct"` // -1 if unknown or no battery
}

func cmdSetup(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print what was detected as JSON")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: pool setup [--json]   describe this device's hardware")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	d := describeDevice(ctx)
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(d)
	}
	printDevice(stdout, d)
	return nil
}

func describeDevice(ctx context.Context) deviceReport {
	d := deviceReport{OS: runtime.GOOS, Arch: runtime.GOARCH, Specs: hwinfo.Detect(), BatteryPct: -1}
	// The tailnet name is what other devices know this one by; fall back to
	// the host name when Tailscale isn't running.
	sctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if st, err := tsutil.GetStatus(sctx); err == nil {
		d.Name = st.Self.Name
	}
	if d.Name == "" {
		h, _ := os.Hostname()
		d.Name = strings.ToLower(h)
	}
	p := sysinfo.NewSampler().Snapshot().Power
	d.PowerKnown, d.HasBattery, d.OnBattery, d.BatteryPct = p.Known, p.HasBattery, p.OnBattery, p.BatteryPct
	return d
}

func printDevice(w io.Writer, d deviceReport) {
	fmt.Fprintf(w, "This device: %s (%s/%s)\n", d.Name, d.OS, d.Arch)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	s := d.Specs
	model := s.CPUModel
	if model == "" {
		model = "unknown"
	}
	fmt.Fprintf(tw, "  CPU\t%s\n", model)
	cores := "unknown"
	if s.PhysicalCores > 0 {
		cores = plural(s.PhysicalCores, "core")
		if s.Hybrid() {
			cores += fmt.Sprintf(" (%d performance + %d efficiency)", s.PerfCores, s.EffCores)
		}
	}
	fmt.Fprintf(tw, "  Cores\t%s, %s\n", cores, plural(s.LogicalCPUs, "thread"))
	mem := "unknown"
	if s.MemoryMB > 0 {
		mem = fmt.Sprintf("%.1f GB", float64(s.MemoryMB)/1024)
	}
	fmt.Fprintf(tw, "  Memory\t%s\n", mem)
	fmt.Fprintf(tw, "  Power\t%s\n", powerSummary(d))
	tw.Flush()
}

func powerSummary(d deviceReport) string {
	switch {
	case !d.PowerKnown:
		return "unknown"
	case !d.HasBattery:
		return "no battery (desktop or server)"
	}
	pct := ""
	if d.BatteryPct >= 0 {
		pct = fmt.Sprintf(", %d%%", d.BatteryPct)
	}
	if d.OnBattery {
		return "laptop, on battery" + pct
	}
	return "laptop, plugged in" + pct
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}
