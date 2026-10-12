package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/montjeffrey/compute-pool/internal/advise"
	"github.com/montjeffrey/compute-pool/internal/capacity"
	"github.com/montjeffrey/compute-pool/internal/client"
	"github.com/montjeffrey/compute-pool/internal/hwinfo"
	"github.com/montjeffrey/compute-pool/internal/proto"
	"github.com/montjeffrey/compute-pool/internal/tsutil"
)

func cmdAdvise(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("advise", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print the analysis as JSON")
	port := fs.Int("port", proto.DefaultPort, "agent TCP port")
	timeout := fs.Duration("timeout", time.Second, "how long to wait for each device")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: pool advise [flags]")
		fmt.Fprintln(stderr, "Compares every device in the pool: what each contributes, how much faster the pool is than this device alone, and what to change.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	st, err := tsutil.GetStatus(ctx)
	if err != nil {
		return err
	}
	c := client.New()
	c.Port = *port
	rep := advise.Analyze(poolDevices(probeAll(ctx, st, c, *timeout)))
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	printAdvice(stdout, rep)
	return nil
}

// poolDevices converts probe results for advise. This device's specs and
// saved measurement are read locally when its agent doesn't report them
// (an older agent, or one started before pool setup measured).
func poolDevices(results []probeResult) []advise.Device {
	var devs []advise.Device
	for _, r := range results {
		d := advise.Device{Name: r.peer.Name, Self: r.peer.Self}
		if r.err != nil {
			d.Err = errors.New(probeStatus(r.err))
			devs = append(devs, d)
			continue
		}
		in := r.info
		d.Version, d.MaxJobs, d.Accepting, d.Reason = in.Version, in.MaxJobs, in.Accepting, in.Reason
		d.PowerKnown, d.HasBattery, d.OnBattery = true, in.HasBattery, in.OnBattery
		d.Specs, d.Capacity = in.Specs, in.Capacity
		if d.Self {
			if d.Specs == nil {
				s := hwinfo.Detect()
				d.Specs = &s
			}
			if d.Capacity == nil {
				if c, err := capacity.Load(capacityPath()); err == nil {
					d.Capacity = c
				}
			}
		}
		devs = append(devs, d)
	}
	return devs
}

func printAdvice(w io.Writer, rep advise.Report) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "DEVICE\tCPU\tLENDS\tCAPACITY\tSHARE\tSTATUS")
	for _, r := range rep.Rows {
		name := r.Name
		if r.Self {
			name += " (this device)"
		}
		cpu, lends, capa, share := r.CPU, "-", "-", "-"
		if cpu == "" {
			cpu = "-"
		}
		if r.Lends > 0 {
			lends = fmt.Sprint(r.Lends)
		}
		if r.Capacity > 0 {
			capa = fmt.Sprintf("%.1f", r.Capacity)
			share = fmt.Sprintf("%.0f%%", 100*r.Share)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", name, cpu, lends, capa, share, r.Status)
	}
	tw.Flush()

	if rep.Reference == "" {
		fmt.Fprintln(w, "\nNo device has been measured yet. Run pool setup on each device, then pool advise again.")
		return
	}
	fmt.Fprintf(w, "\nCapacity is work per second in units of one job on %s.\n", rep.Reference)
	if rep.Speedup > 0 {
		fmt.Fprintf(w, "Pool capacity %.1f, against %.1f for this device alone: CPU-bound batches can run up to %.2f× as fast\n",
			rep.Total, rep.SelfCapacity, rep.Speedup)
		fmt.Fprintln(w, "using the whole pool (in tests, long batches reached about 90% of that; short tasks gain less).")
	} else {
		fmt.Fprintf(w, "Pool capacity %.1f. This device isn't measured or isn't lending, so there is no speedup to compare against.\n", rep.Total)
	}
	if rep.Available < rep.Total {
		fmt.Fprintf(w, "Right now some devices are paused, leaving %.1f (%.0f%%) available.\n", rep.Available, 100*rep.Available/rep.Total)
	}
	if rep.RecommendedSpeedup > 0 && rep.RecommendedTotal > rep.Total*1.01 {
		fmt.Fprintf(w, "With every device on pool setup's recommended settings: capacity %.1f, up to %.2f× as fast as this device alone.\n",
			rep.RecommendedTotal, rep.RecommendedSpeedup)
	}

	var advice []string
	for _, r := range rep.Rows {
		for _, a := range r.Advice {
			advice = append(advice, r.Name+": "+a)
		}
	}
	if len(advice) > 0 {
		fmt.Fprintln(w, "\nAdvice:")
		for _, a := range advice {
			fmt.Fprintln(w, "  "+a)
		}
	}
}
