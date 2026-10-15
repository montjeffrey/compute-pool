package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/montjeffrey/compute-pool/internal/capacity"
	"github.com/montjeffrey/compute-pool/internal/client"
	"github.com/montjeffrey/compute-pool/internal/hwinfo"
	"github.com/montjeffrey/compute-pool/internal/proto"
	"github.com/montjeffrey/compute-pool/internal/recommend"
	"github.com/montjeffrey/compute-pool/internal/sysinfo"
	"github.com/montjeffrey/compute-pool/internal/tsutil"
)

// capacityPath is where measurements are saved; tests point it elsewhere.
var capacityPath = capacity.DefaultPath

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
	// PoolDevices are this owner's other devices that run a pool agent: the
	// default devices to allow. Devices without an agent aren't included,
	// so setup never widens access beyond the pool on its own.
	PoolDevices []string `json:"pool_devices,omitempty"`
}

// setupReport is pool setup's --json output.
type setupReport struct {
	Device         deviceReport     `json:"device"`
	Capacity       *proto.Capacity  `json:"capacity,omitempty"`
	Recommendation recommend.Result `json:"recommendation"`
	Command        []string         `json:"command"`
}

func cmdSetup(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print the device, measurement and recommendation as JSON, and change nothing")
	yes := fs.Bool("yes", false, "apply the recommendation without asking")
	noMeasure := fs.Bool("no-measure", false, "use the saved measurement (or the specs alone) instead of measuring")
	measureTime := fs.Duration("measure-time", 2*time.Second, "how long to measure each number of parallel jobs")
	allowFlag := fs.String("allow", "", "devices allowed to send jobs (default: your other devices running a pool agent)")
	keep := fs.Int("keep", 0, "hardware threads to keep free for your own work (default: a quarter, at least 2)")
	jobMemory := fs.Int64("job-memory", recommend.DefaultJobMemoryMB, "memory each job needs, in MB")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: pool setup [flags]")
		fmt.Fprintln(stderr, "Describes this device, measures its speed, recommends agent settings and, once you confirm, installs the agent with them.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if *measureTime < 100*time.Millisecond {
		return errors.New("--measure-time must be at least 100ms")
	}

	say := stdout
	if *asJSON {
		say = io.Discard
	}
	d := describeDevice(ctx)
	printDevice(say, d)

	var measured *proto.Capacity
	baseline := -1.0
	if *noMeasure {
		c, err := capacity.Load(capacityPath())
		switch {
		case err == nil:
			measured = c
			fmt.Fprintf(say, "\nUsing the measurement from %s.\n", c.MeasuredAt.Local().Format("2006-01-02 15:04"))
		case errors.Is(err, os.ErrNotExist):
			fmt.Fprintln(say, "\nNo saved measurement, so the recommendation uses the specs alone.")
		default:
			fmt.Fprintf(say, "\nIgnoring the saved measurement: %v\n", err)
		}
	} else {
		levels := capacity.Levels(d.Specs)
		est := time.Duration(len(levels))*(*measureTime) + *measureTime/4 + time.Second
		fmt.Fprintf(say, "\nMeasuring speed with 1 to %d jobs at once. This takes about %s and uses all of the CPU (Ctrl+C to stop).\n",
			levels[len(levels)-1], est.Round(time.Second))
		baseline = baselineBusy(ctx)
		var single float64
		c, err := capacity.Measure(ctx, levels, *measureTime, func(l proto.Level) {
			if single == 0 {
				single = l.Rate
			}
			fmt.Fprintf(say, "  %-9s %4.1f× one job\n", plural(l.Jobs, "job"), l.Rate/single)
		})
		if err != nil {
			return fmt.Errorf("measure: %w", err)
		}
		c.OnBattery = d.OnBattery
		measured = &c
		if err := capacity.Save(capacityPath(), c); err != nil {
			fmt.Fprintf(stderr, "pool: couldn't save the measurement: %v\n", err)
		}
	}

	rec := recommend.Recommend(recommend.Input{
		Specs:           d.Specs,
		PowerKnown:      d.PowerKnown,
		HasBattery:      d.HasBattery,
		Capacity:        measured,
		BaselineBusyPct: baseline,
		KeepThreads:     *keep,
		JobMemoryMB:     *jobMemory,
	})
	allow, allowWhy := d.PoolDevices, "your other devices running a pool agent"
	if *allowFlag != "" {
		allow, allowWhy = splitList(*allowFlag), "from --allow"
	}
	installArgs := rec.Args(allow)
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(setupReport{Device: d, Capacity: measured, Recommendation: rec, Command: append([]string{"pool", "install"}, installArgs...)})
	}
	printRecommendation(stdout, rec, allow, allowWhy)

	if runtime.GOOS != "windows" {
		fmt.Fprintf(stdout, "\nTo lend this device, run the agent with these settings (pool install is Windows-only so far):\n  %s\n", agentCommand(installArgs, defaultAgentLog()))
		return nil
	}
	fmt.Fprintf(stdout, "\nCommand: pool install %s\n", strings.Join(installArgs, " "))
	if len(allow) == 0 {
		fmt.Fprintln(stdout, "None of your other devices runs a pool agent yet, so nothing would be allowed to send jobs here.")
		fmt.Fprintln(stdout, "Run pool setup again with --allow and the names of the devices that should send jobs.")
		return nil
	}
	if !isElevated() {
		fmt.Fprintln(stdout, "This shell isn't running as administrator, so the agent will start when you log in rather than at boot.")
		fmt.Fprintln(stdout, "To start it at boot, run pool setup from an administrator shell.")
	}
	if !*yes {
		fmt.Fprint(stdout, "Install the agent with these settings? [y/N] ")
		answer, _ := bufio.NewReader(stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
			fmt.Fprintln(stdout, "Nothing changed.")
			return nil
		}
	}
	return cmdInstall(ctx, installArgs, stdout, stderr)
}

func describeDevice(ctx context.Context) deviceReport {
	d := deviceReport{OS: runtime.GOOS, Arch: runtime.GOARCH, Specs: hwinfo.Detect(), BatteryPct: -1}
	// The tailnet name is what other devices know this one by; fall back to
	// the host name when Tailscale isn't running.
	sctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if st, err := tsutil.GetStatus(sctx); err == nil {
		d.Name = st.Self.Name
		for _, r := range probeAll(sctx, st, client.New(), time.Second) {
			if !r.peer.Self && r.err == nil && r.peer.Name != "" {
				d.PoolDevices = append(d.PoolDevices, r.peer.Name)
			}
		}
	}
	if d.Name == "" {
		h, _ := os.Hostname()
		d.Name = strings.ToLower(h)
	}
	p := sysinfo.NewSampler().Snapshot().Power
	d.PowerKnown, d.HasBattery, d.OnBattery, d.BatteryPct = p.Known, p.HasBattery, p.OnBattery, p.BatteryPct
	return d
}

// baselineBusy samples CPU use for about a second before measuring, or
// returns -1 where the platform can't tell.
func baselineBusy(ctx context.Context) float64 {
	s := sysinfo.NewSampler()
	sctx, cancel := context.WithTimeout(ctx, 1100*time.Millisecond)
	defer cancel()
	s.Run(sctx, 250*time.Millisecond)
	return s.Snapshot().CPUBusyPct
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

func printRecommendation(w io.Writer, r recommend.Result, allow []string, allowWhy string) {
	fmt.Fprintln(w, "\nRecommended settings:")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, s := range r.Settings {
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", s.Name, s.Value, s.Reason)
	}
	if len(allow) > 0 {
		fmt.Fprintf(tw, "  allowed devices\t%s\t%s\n", strings.Join(allow, ", "), allowWhy)
	}
	tw.Flush()
	for _, warning := range r.Warnings {
		fmt.Fprintf(w, "Note: %s.\n", warning)
	}
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

func splitList(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// agentCommand is the pool agent command line pool setup suggests where
// pool install isn't available. It logs to a file, as pool install's agent
// does, so pool report can read the log through the agent.
func agentCommand(args []string, logPath string) string {
	parts := append([]string{"pool", "agent"}, args...)
	parts = append(parts, "--log-file", logPath)
	for i, p := range parts {
		if p == "" || strings.ContainsAny(p, " \t'\"$\\") {
			parts[i] = "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
		}
	}
	return strings.Join(parts, " ")
}
