package report

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"
)

// WriteText prints rep for people.
func WriteText(w io.Writer, rep Report) {
	for i, d := range rep.Devices {
		if i > 0 {
			fmt.Fprintln(w)
		}
		writeDevice(w, d)
	}
}

const stamp = "2006-01-02 15:04"

func writeDevice(w io.Writer, d DeviceReport) {
	title := d.Node
	if title == "" {
		title = "(unknown device)"
	}
	if d.From.IsZero() {
		fmt.Fprintf(w, "=== %s: the log is empty\n", title)
		return
	}
	fmt.Fprintf(w, "=== %s: log from %s to %s, %d jobs\n", title, d.From.Format(stamp), d.To.Format(stamp), d.Reliability.Jobs)
	if len(d.Workloads) == 0 {
		fmt.Fprintln(w, "\nNo command ran often enough to assess its speed (see --min-jobs).")
	}
	for _, wr := range d.Workloads {
		writeWorkload(w, wr)
	}
	writeReliability(w, d.Reliability)
}

func writeWorkload(w io.Writer, wr WorkloadReport) {
	fmt.Fprintf(w, "\n--- %s  (%d successful jobs in %d runs)\n", wr.Command, wr.Jobs, wr.Runs)

	fmt.Fprintln(w, "\n1. Speed by jobs at once")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	header := "JOBS\tTASKS\tTIME/TASK\tP90\tSLOWDOWN\tTHROUGHPUT"
	if wr.CapacityKernel != "" {
		header += "\tSETUP PREDICTS"
	}
	fmt.Fprintln(tw, "   "+header)
	for _, l := range wr.Scaling {
		row := fmt.Sprintf("   %d\t%d\t%.2fs\t%.2fs\t%.2f×\t%.2f/s", l.Jobs, l.Count, l.Median, l.P90, l.Slowdown, l.Throughput)
		if wr.CapacityKernel != "" && l.Predicted > 0 {
			row += fmt.Sprintf("\t%.2f/s (%+.0f%%)", l.Predicted, 100*(l.Throughput/l.Predicted-1))
		}
		fmt.Fprintln(tw, row)
	}
	tw.Flush()
	if n := len(wr.Scaling); n >= 2 {
		lo, hi := wr.Scaling[0], wr.Scaling[n-1]
		fmt.Fprintf(w, "   %d→%d jobs at once: %.1f× the slots gave %.2f× the throughput; each task took %.0f%% longer.\n",
			lo.Jobs, hi.Jobs, float64(hi.Jobs)/float64(lo.Jobs), hi.Throughput/lo.Throughput, 100*(hi.Slowdown-1))
	}
	if wr.CapacityKernel != "" {
		fmt.Fprintf(w, "   SETUP PREDICTS scales the first row by pool setup's measurement (%s); the %% is real use against it.\n", wr.CapacityKernel)
	}

	fmt.Fprintln(w, "\n2. Consistency between runs")
	if len(wr.Consistency) == 0 {
		fmt.Fprintln(w, "   Not enough runs at the same number of jobs at once (need 3).")
	}
	for _, c := range wr.Consistency {
		fmt.Fprintf(w, "   %d jobs at once, %d runs: median %.2fs per task, most runs %.2f–%.2fs. Differences under ±%.0f%% are noise.\n",
			c.Jobs, c.Runs, c.Median, c.Low, c.High, 100*c.Noise)
		if len(c.ByVersion) > 0 {
			parts := make([]string, len(c.ByVersion))
			for i, v := range c.ByVersion {
				parts[i] = fmt.Sprintf("%s %.2fs (%d)", v.Version, v.Median, v.Count)
			}
			fmt.Fprintf(w, "     by agent version: %s\n", strings.Join(parts, ", "))
		}
	}

	fmt.Fprintln(w, "\n3. Slot use")
	if s := wr.SlotUse; s == nil {
		fmt.Fprintln(w, "   No run had at least twice as many tasks as job slots.")
	} else {
		fmt.Fprintf(w, "   %d runs: slots busy %.0f%% of the run (median), lowest %.0f%% (run at %s).\n",
			s.Runs, 100*s.MedianUtil, 100*s.WorstUtil, s.WorstAt.Format(stamp))
		fmt.Fprintf(w, "   Slowest task vs the run's median: %.2f× (median), worst %.2f× (run at %s).\n",
			s.MedianStraggler, s.WorstStraggler, s.WorstStragglerAt.Format(stamp))
	}
}

func writeReliability(w io.Writer, r Reliability) {
	fmt.Fprintln(w, "\n4. Reliability (all jobs)")
	if r.Jobs > 0 {
		pct := func(n int) string { return fmt.Sprintf("%d (%.1f%%)", n, 100*float64(n)/float64(r.Jobs)) }
		fmt.Fprintf(w, "   %d jobs: %s succeeded, %s failed in the program", r.Jobs, pct(r.OK), pct(r.Failed))
		extra := []struct {
			n    int
			what string
		}{{r.NoStart, "couldn't start"}, {r.TimedOut, "timed out"}, {r.Canceled, "canceled"}, {r.Lost, "lost when the agent restarted"}, {r.Running, "still running"}}
		for _, e := range extra {
			if e.n > 0 {
				fmt.Fprintf(w, ", %s %s", pct(e.n), e.what)
			}
		}
		fmt.Fprintln(w, ".")
	}
	fmt.Fprintf(w, "   Agent started %d times", r.AgentStarts)
	if n := r.Pauses - r.Unended; n > 0 {
		fmt.Fprintf(w, "; took no jobs %d times (job limit 0) for %s in all", n, r.Paused.Round(time.Second))
	}
	if r.Unended > 0 {
		fmt.Fprintf(w, "; was taking no jobs when it stopped %d times", r.Unended)
	}
	if r.Refused > 0 {
		fmt.Fprintf(w, "; refused %d requests from devices that aren't allowed", r.Refused)
	}
	fmt.Fprintln(w, ".")
}
