// Command pool shares CPU between your own devices over Tailscale.
//
//	pool agent --allow desktop,laptop   lend this device's CPU
//	pool nodes                            list devices running an agent
//	pool run --node laptop -- go test ./... run a command on another device
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/montjeffrey/compute-pool/internal/client"
	"github.com/montjeffrey/compute-pool/internal/proto"
	"github.com/montjeffrey/compute-pool/internal/tsutil"
)

const usage = `pool shares CPU between your own devices over Tailscale.

Usage:
  pool setup [flags]            measure this device and recommend (and apply)
                                settings for its agent
  pool agent [flags]            lend this device's CPU to allowed devices
  pool nodes [flags]            list tailnet devices running an agent
  pool run [--node N] [flags] -- command [args...]
                                run a command on the least-loaded device (or on N)
                                and stream its output
  pool map (--range A..B | --items FILE | --inputs GLOB) [flags] -- command {} ...
                                run one task per item, spread over every device
  pool dashboard [flags]        live web view of every device and its jobs
  pool install [agent flags]    start the agent automatically (Windows)
  pool uninstall                stop and remove the automatic agent
  pool version

Run "pool <command> -h" for a command's flags.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var err error
	code := 0
	switch args[0] {
	case "setup":
		err = cmdSetup(ctx, args[1:], os.Stdin, stdout, stderr)
	case "agent":
		err = cmdAgent(ctx, args[1:], stderr)
	case "nodes":
		err = cmdNodes(ctx, args[1:], stdout)
	case "run":
		code, err = cmdRun(ctx, args[1:], stdout, stderr)
	case "map":
		code, err = cmdMap(ctx, args[1:], stdout, stderr)
	case "install":
		err = cmdInstall(ctx, args[1:], stdout, stderr)
	case "uninstall":
		err = cmdUninstall(ctx, args[1:], stdout, stderr)
	case "dashboard":
		err = cmdDashboard(ctx, args[1:], stdout, stderr)
	case "version":
		fmt.Fprintf(stdout, "pool %s %s/%s\n", proto.Version, runtime.GOOS, runtime.GOARCH)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
	default:
		fmt.Fprintf(stderr, "pool: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "pool: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	return code
}

func cmdNodes(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("nodes", flag.ContinueOnError)
	port := fs.Int("port", proto.DefaultPort, "agent TCP port")
	timeout := fs.Duration("timeout", time.Second, "how long to wait for each device")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := tsutil.GetStatus(ctx)
	if err != nil {
		return err
	}
	c := client.New()
	c.Port = *port

	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "DEVICE\tOS\tCPUS\tIDLE\tLOAD\tJOBS\tPOWER\tSTATUS")
	for _, r := range probeAll(ctx, st, c, *timeout) {
		name := r.peer.Name
		if r.peer.Self {
			name += " (this device)"
		}
		if r.err != nil {
			fmt.Fprintf(tw, "%s\t%s\t-\t-\t-\t-\t-\t%s\n", name, r.peer.OS, probeStatus(r.err))
			continue
		}
		in := r.info
		load, idle := "?", "?"
		if in.CPUBusyPct >= 0 {
			load = fmt.Sprintf("%.0f%%", in.CPUBusyPct)
			idle = fmt.Sprintf("%.1f", in.IdleCPUs)
		}
		status := "ready"
		if !in.Accepting {
			status = "paused: " + in.Reason
		}
		fmt.Fprintf(tw, "%s\t%s/%s\t%d\t%s\t%s\t%d/%d\t%s\t%s\n", name, in.OS, in.Arch, in.LogicalCPUs,
			idle, load, in.Running, in.MaxJobs, describePower(in.HasBattery, in.OnBattery, in.BatteryPct), status)
	}
	return tw.Flush()
}

type probeResult struct {
	peer tsutil.Peer
	info proto.Info
	err  error
}

// resolveOwned finds a device by name or Tailscale IP and refuses any that
// this device's owner does not own, so jobs and their data never reach a
// device that someone else controls.
func resolveOwned(st tsutil.Status, name string) (tsutil.Peer, error) {
	p, err := st.Resolve(name)
	if err != nil {
		return tsutil.Peer{}, err
	}
	if !st.IsOwned(p) {
		return tsutil.Peer{}, fmt.Errorf("%s is not one of your devices (it belongs to another user or to tags); pool only sends work to devices you own", p.Name)
	}
	return p, nil
}

// probeAll asks every online device this device's owner also owns for its
// agent's status, in parallel, and returns the answers sorted by name.
func probeAll(ctx context.Context, st tsutil.Status, c *client.Client, timeout time.Duration) []probeResult {
	var (
		mu  sync.Mutex
		out []probeResult
		wg  sync.WaitGroup
	)
	for _, p := range st.Owned() {
		if !p.Online || p.IPv4 == "" {
			continue
		}
		wg.Add(1)
		go func(p tsutil.Peer) {
			defer wg.Done()
			pctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			info, err := c.Info(pctx, p.IPv4)
			mu.Lock()
			out = append(out, probeResult{p, info, err})
			mu.Unlock()
		}(p)
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool { return out[i].peer.Name < out[j].peer.Name })
	return out
}

func probeStatus(err error) string {
	if errors.Is(err, client.ErrForbidden) {
		return "agent refuses this device (add it to --allow)"
	}
	return "no agent"
}

func describePower(hasBattery, onBattery bool, pct int) string {
	if !hasBattery {
		return "AC"
	}
	src := "AC"
	if onBattery {
		src = "battery"
	}
	if pct >= 0 {
		return fmt.Sprintf("%s %d%%", src, pct)
	}
	return src
}

// pickTargets ranks every device that can take a job now. When none can, the
// error says why for each device.
func pickTargets(ctx context.Context, c *client.Client, timeout time.Duration) ([]client.Candidate, error) {
	st, err := tsutil.GetStatus(ctx)
	if err != nil {
		return nil, err
	}
	results := probeAll(ctx, st, c, timeout)
	var cands []client.Candidate
	for _, r := range results {
		if r.err == nil {
			cands = append(cands, client.Candidate{Name: r.peer.Name, Host: r.peer.IPv4, Info: r.info})
		}
	}
	if ranked := client.Rank(cands); len(ranked) > 0 {
		return ranked, nil
	}
	return nil, fmt.Errorf("no device can take the job right now (%s)", explain(results))
}

// explain says, per device, why it cannot take work.
func explain(results []probeResult) string {
	if len(results) == 0 {
		return "no online devices"
	}
	var why []string
	for _, r := range results {
		reason := probeStatus(r.err)
		if r.err == nil {
			reason = r.info.Reason
		}
		why = append(why, r.peer.Name+": "+reason)
	}
	return strings.Join(why, "; ")
}

type envFlag map[string]string

func (e envFlag) String() string { return "" }

func (e envFlag) Set(s string) error {
	k, v, ok := strings.Cut(s, "=")
	if !ok || k == "" {
		return fmt.Errorf("want KEY=VALUE, got %q", s)
	}
	e[k] = v
	return nil
}

func cmdRun(ctx context.Context, args []string, stdout, stderr io.Writer) (int, error) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	node := fs.String("node", "auto", `device to run on: tailnet name, Tailscale IP, or "auto" for the least-loaded device`)
	probeTimeout := fs.Duration("probe-timeout", time.Second, "with --node auto, how long to wait for each device's status")
	shell := fs.Bool("shell", false, "run through the device's shell (cmd.exe on Windows, /bin/sh elsewhere)")
	timeout := fs.Duration("timeout", 0, "kill the job after this long (0 = no limit)")
	port := fs.Int("port", proto.DefaultPort, "agent TCP port")
	verbose := fs.Bool("v", false, "print job id, device and duration")
	env := envFlag{}
	fs.Var(env, "env", "set an environment variable for the job (KEY=VALUE, repeatable)")
	var ship, collect listFlag
	fs.Var(&ship, "ship", "file or directory to send with the job (repeatable)")
	fs.Var(&collect, "collect", "files to bring back (glob relative to the job folder, repeatable)")
	outDir := fs.String("out", "pool-out", "where collected files go")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: pool run [--node N] [flags] -- command [args...]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	argv := fs.Args()
	if len(argv) == 0 {
		fs.Usage()
		return 2, errors.New("a command is required")
	}

	c := client.New()
	c.Port = *port
	auto := *node == "" || *node == "auto"
	var targets []client.Candidate
	if auto {
		var err error
		if targets, err = pickTargets(ctx, c, *probeTimeout); err != nil {
			return 1, err
		}
	} else {
		// Even a raw IP is looked up, so it must be a tailnet device you own.
		st, err := tsutil.GetStatus(ctx)
		if err != nil {
			return 1, err
		}
		p, err := resolveOwned(st, *node)
		if err != nil {
			return 1, err
		}
		if !p.Online {
			return 1, fmt.Errorf("%s is offline", p.Name)
		}
		targets = []client.Candidate{{Name: p.Name, Host: p.IPv4}}
	}

	req := proto.RunRequest{Argv: argv, Shell: *shell, TimeoutSec: int(timeout.Seconds()), Collect: collect}
	if len(env) > 0 {
		req.Env = env
	}
	files, err := loadShipped(ship)
	if err != nil {
		return 1, err
	}
	req.Files = files
	out := client.Output{Stdout: stdout, Stderr: stderr}
	if len(collect) > 0 {
		out.Dir = *outDir
	}
	for i, t := range targets {
		if auto && *verbose {
			fmt.Fprintf(stderr, "pool: placing job on %s (%.1f of %d CPUs idle, %d/%d jobs)\n",
				t.Name, t.Info.IdleCPUs, t.Info.LogicalCPUs, t.Info.Running, t.Info.MaxJobs)
		}
		res, err := c.Run(ctx, t.Host, req, out)
		if err != nil {
			// A refusal or failed connection happens before anything runs, so
			// moving on is safe.
			if (errors.Is(err, client.ErrBusy) || errors.Is(err, client.ErrUnreachable)) && i < len(targets)-1 {
				if *verbose {
					fmt.Fprintf(stderr, "pool: %s declined (%v); trying the next device\n", t.Name, err)
				}
				continue
			}
			if errors.Is(err, context.Canceled) {
				return 130, errors.New("canceled; the job was stopped on the remote device")
			}
			return 1, fmt.Errorf("%s: %w", t.Name, err)
		}
		if res.RemoteError != "" {
			fmt.Fprintf(stderr, "pool: %s: %s\n", res.Node, res.RemoteError)
		}
		if *verbose {
			fmt.Fprintf(stderr, "pool: job %s on %s exited %d after %s\n", res.JobID, res.Node, res.ExitCode, res.Duration)
		}
		if len(res.Files) > 0 {
			fmt.Fprintf(stderr, "pool: %d file(s) collected into %s\n", len(res.Files), *outDir)
		}
		return res.ExitCode, nil
	}
	return 1, errors.New("no device accepted the job")
}

func defaultWorkDir() string {
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, "compute-pool", "jobs")
}
