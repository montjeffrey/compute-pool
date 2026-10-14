package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/montjeffrey/compute-pool/internal/agent"
	"github.com/montjeffrey/compute-pool/internal/capacity"
	"github.com/montjeffrey/compute-pool/internal/client"
	"github.com/montjeffrey/compute-pool/internal/proto"
	"github.com/montjeffrey/compute-pool/internal/report"
	"github.com/montjeffrey/compute-pool/internal/tsutil"
)

// logFetchTimeout bounds downloading one device's log (up to ~20 MB).
const logFetchTimeout = time.Minute

// localAgentLog is this device's agent log when the agent can't serve it;
// tests point it elsewhere.
var localAgentLog = installedAgentLog

func cmdReport(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var logs listFlag
	fs.Var(&logs, "log", "assess this agent log file instead of asking the devices (repeatable)")
	nodes := fs.String("nodes", "", "comma-separated devices to assess (default: every device running an agent)")
	asJSON := fs.Bool("json", false, "print the assessments as JSON")
	minJobs := fs.Int("min-jobs", 30, "successful jobs a command needs before its speed is assessed")
	runGap := fs.Duration("run-gap", 2*time.Second, "idle time between jobs that separates one run from the next")
	port := fs.Int("port", proto.DefaultPort, "agent TCP port")
	timeout := fs.Duration("timeout", time.Second, "how long to wait for each device to answer")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: pool report [flags]")
		fmt.Fprintln(stderr, "Assesses past use from each device's agent log: speed by jobs at once (against pool setup's")
		fmt.Fprintln(stderr, "measurement), consistency between runs, slot use, and reliability.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if len(logs) > 0 && *nodes != "" {
		return errors.New("use --log or --nodes, not both")
	}

	var (
		devs []report.Device
		err  error
	)
	if len(logs) > 0 {
		devs, err = reportFromFiles(logs)
	} else {
		devs, err = reportFromPool(ctx, *nodes, *port, *timeout, stderr)
	}
	if err != nil {
		return err
	}
	if len(devs) == 0 {
		return errors.New("no agent log to assess")
	}
	rep := report.Analyze(devs, report.Options{MinJobs: *minJobs, RunGap: *runGap})
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	report.WriteText(stdout, rep)
	return nil
}

// reportFromFiles parses log files. This device's pool setup measurement is
// attached to a log whose device name matches its host name.
func reportFromFiles(paths []string) ([]report.Device, error) {
	host, _ := os.Hostname()
	var devs []report.Device
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			return nil, err
		}
		lg, err := report.Parse(f)
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		if lg.Node == "" {
			lg.Node = strings.TrimSuffix(filepath.Base(p), filepath.Ext(p))
		}
		d := report.Device{Log: lg}
		if strings.EqualFold(lg.Node, host) {
			d.Capacity, _ = capacity.Load(capacityPath())
		}
		devs = append(devs, d)
	}
	return devs, nil
}

// reportFromPool fetches every chosen device's log from its agent. For this
// device, a log the agent can't serve is read from where pool install keeps
// it. Devices whose log can't be had are reported on stderr and skipped.
func reportFromPool(ctx context.Context, only string, port int, timeout time.Duration, stderr io.Writer) ([]report.Device, error) {
	st, err := tsutil.GetStatus(ctx)
	if err != nil {
		return nil, err
	}
	c := client.New()
	c.Port = port
	results := probeAll(ctx, st, c, timeout)
	if only != "" {
		want := map[string]bool{}
		for _, n := range strings.Split(only, ",") {
			if n = strings.TrimSpace(n); n != "" {
				want[strings.ToLower(n)] = true
			}
		}
		var kept []probeResult
		for _, r := range results {
			if want[strings.ToLower(r.peer.Name)] {
				kept = append(kept, r)
				delete(want, strings.ToLower(r.peer.Name))
			}
		}
		missing := make([]string, 0, len(want))
		for n := range want {
			missing = append(missing, n)
		}
		sort.Strings(missing)
		for _, n := range missing {
			fmt.Fprintf(stderr, "pool report: %s is not one of your online devices\n", n)
		}
		results = kept
	} else {
		// An offline device's log stays on it: say so, or it seems forgotten.
		for _, p := range st.Owned() {
			if !p.Online {
				fmt.Fprintf(stderr, "pool report: %s: offline; skipped\n", p.Name)
			}
		}
	}

	devs := make([]*report.Device, len(results))
	problems := make([]string, len(results))
	var wg sync.WaitGroup
	for i, r := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			devs[i], problems[i] = fetchDevice(ctx, c, r)
		}()
	}
	wg.Wait()

	var out []report.Device
	for i, d := range devs {
		if d != nil {
			out = append(out, *d)
		}
		if problems[i] != "" {
			fmt.Fprintf(stderr, "pool report: %s: %s\n", results[i].peer.Name, problems[i])
		}
	}
	return out, nil
}

func fetchDevice(ctx context.Context, c *client.Client, r probeResult) (*report.Device, string) {
	var (
		data []byte
		err  = r.err
	)
	if err == nil {
		lctx, cancel := context.WithTimeout(ctx, logFetchTimeout)
		data, err = c.Log(lctx, r.peer.IPv4)
		cancel()
	}
	if err != nil && r.peer.Self {
		if p := localAgentLog(); p != "" {
			if b, lerr := agent.ReadLog(p); lerr == nil {
				data, err = b, nil
			}
		}
	}
	if err != nil {
		return nil, logProblem(r, err)
	}
	lg, err := report.Parse(bytes.NewReader(data))
	if err != nil {
		return nil, "can't read its log: " + err.Error()
	}
	if lg.Node == "" {
		lg.Node = r.peer.Name
	}
	d := &report.Device{Log: lg}
	if r.err == nil {
		d.Capacity = r.info.Capacity
	}
	if d.Capacity == nil && r.peer.Self {
		d.Capacity, _ = capacity.Load(capacityPath())
	}
	return d, ""
}

func logProblem(r probeResult, err error) string {
	switch {
	case r.err != nil:
		return probeStatus(r.err) + "; skipped"
	case errors.Is(err, client.ErrNoLog) && strings.Contains(err.Error(), "404 page not found"):
		return fmt.Sprintf("agent %s is too old to share its log; update it (pool install), or copy its agent.log here and use --log", r.info.Version)
	case errors.Is(err, client.ErrNoLog):
		return err.Error()
	default:
		return "can't fetch its log: " + err.Error()
	}
}
