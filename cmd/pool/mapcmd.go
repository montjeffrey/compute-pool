package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/montjeffrey/compute-pool/internal/client"
	"github.com/montjeffrey/compute-pool/internal/fanout"
	"github.com/montjeffrey/compute-pool/internal/proto"
	"github.com/montjeffrey/compute-pool/internal/tsutil"
)

// maxShipBytes leaves room for base64 and JSON overhead under the agent's
// request limit.
const maxShipBytes = 180 << 20

const maxTasks = 100000

type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(s string) error { *l = append(*l, s); return nil }

// loadShipped reads local files and directories to send with a job. A file
// is shipped under its base name; a directory keeps its name and layout.
func loadShipped(paths []string) ([]proto.InputFile, error) {
	var out []proto.InputFile
	var total int64
	add := func(rel string, src string) error {
		data, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		if total += int64(len(data)); total > maxShipBytes {
			return fmt.Errorf("shipped files exceed %d MiB", maxShipBytes>>20)
		}
		// The hash lets agents cache the file, so later jobs skip resending it.
		sum := sha256.Sum256(data)
		out = append(out, proto.InputFile{Path: rel, Data: data, SHA256: hex.EncodeToString(sum[:])})
		return nil
	}
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			if err := add(filepath.Base(p), p); err != nil {
				return nil, err
			}
			continue
		}
		base := filepath.Base(filepath.Clean(p))
		err = filepath.WalkDir(p, func(f string, d fs.DirEntry, err error) error {
			if err != nil || !d.Type().IsRegular() {
				return err
			}
			rel, err := filepath.Rel(p, f)
			if err != nil {
				return err
			}
			return add(path.Join(base, filepath.ToSlash(rel)), f)
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// buildTasks turns exactly one of --range, --items or --inputs into tasks.
// For --inputs it also returns the local file behind each task.
func buildTasks(rangeSpec, itemsFile string, inputs []string, stdin io.Reader) ([]fanout.Task, map[int]string, error) {
	given := 0
	for _, set := range []bool{rangeSpec != "", itemsFile != "", len(inputs) > 0} {
		if set {
			given++
		}
	}
	if given != 1 {
		return nil, nil, errors.New("give exactly one of --range, --items or --inputs")
	}

	var items []string
	local := map[int]string{}
	switch {
	case rangeSpec != "":
		lo, hi, ok := strings.Cut(rangeSpec, "..")
		a, errA := strconv.Atoi(strings.TrimSpace(lo))
		b, errB := strconv.Atoi(strings.TrimSpace(hi))
		if !ok || errA != nil || errB != nil || a > b {
			return nil, nil, fmt.Errorf("--range wants FROM..TO, e.g. 1..100; got %q", rangeSpec)
		}
		if b-a+1 > maxTasks {
			return nil, nil, fmt.Errorf("--range gives %d tasks; the limit is %d", b-a+1, maxTasks)
		}
		for i := a; i <= b; i++ {
			items = append(items, strconv.Itoa(i))
		}
	case itemsFile != "":
		r := stdin
		if itemsFile != "-" {
			f, err := os.Open(itemsFile)
			if err != nil {
				return nil, nil, err
			}
			defer f.Close()
			r = f
		}
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			if line := strings.TrimSpace(sc.Text()); line != "" {
				items = append(items, line)
			}
		}
		if err := sc.Err(); err != nil {
			return nil, nil, err
		}
	default:
		seen := map[string]bool{}
		var files []string
		for _, pat := range inputs {
			matches, err := filepath.Glob(pat)
			if err != nil {
				return nil, nil, fmt.Errorf("--inputs %q: %w", pat, err)
			}
			for _, m := range matches {
				if info, err := os.Stat(m); err == nil && info.Mode().IsRegular() && !seen[m] {
					seen[m] = true
					files = append(files, m)
				}
			}
		}
		sort.Strings(files)
		for i, f := range files {
			items = append(items, filepath.Base(f))
			local[i+1] = f
		}
	}
	if len(items) == 0 {
		return nil, nil, errors.New("no tasks: the input list is empty")
	}
	if len(items) > maxTasks {
		return nil, nil, fmt.Errorf("%d tasks; the limit is %d", len(items), maxTasks)
	}
	tasks := make([]fanout.Task, len(items))
	for i, it := range items {
		tasks[i] = fanout.Task{Index: i + 1, Item: it}
	}
	return tasks, local, nil
}

// substitute replaces {} with the task's item and {#} with its index.
func substitute(argv []string, t fanout.Task) []string {
	out := make([]string, len(argv))
	idx := strconv.Itoa(t.Index)
	for i, a := range argv {
		a = strings.ReplaceAll(a, "{#}", idx)
		out[i] = strings.ReplaceAll(a, "{}", t.Item)
	}
	return out
}

func cmdMap(ctx context.Context, args []string, stdout, stderr io.Writer) (int, error) {
	fs := flag.NewFlagSet("map", flag.ContinueOnError)
	fs.SetOutput(stderr)
	rangeSpec := fs.String("range", "", "one task per number, e.g. 1..100")
	itemsFile := fs.String("items", "", "one task per non-empty line of this file (- for stdin)")
	var inputs, ship, collect listFlag
	fs.Var(&inputs, "inputs", "one task per matching local file, shipped with its task (glob, repeatable)")
	fs.Var(&ship, "ship", "file or directory sent with every task (repeatable)")
	fs.Var(&collect, "collect", "files to bring back from each task (glob relative to the job folder, repeatable)")
	outDir := fs.String("out", "pool-out", "where collected files go, one folder per task")
	nodes := fs.String("nodes", "", "comma-separated devices to use (default: every device with an agent)")
	jobs := fs.Int("jobs", 0, "max tasks running at once across all devices (0 = every free slot)")
	retries := fs.Int("retries", 2, "times to retry a task elsewhere if its device fails")
	shell := fs.Bool("shell", false, "run each task through the device's shell")
	timeout := fs.Duration("timeout", 0, "kill a task after this long (0 = no limit)")
	keepOrder := fs.Bool("keep-order", false, "print output in input order instead of as tasks finish")
	speculate := fs.Bool("speculate", true, "near the end, start backup copies of slow tasks on other devices (turn off for tasks with side effects)")
	port := fs.Int("port", proto.DefaultPort, "agent TCP port")
	probeTimeout := fs.Duration("probe-timeout", time.Second, "how long to wait for each device's status")
	verbose := fs.Bool("v", false, "print a line per task with its device and duration")
	env := envFlag{}
	fs.Var(env, "env", "environment variable for every task (KEY=VALUE, repeatable)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: pool map (--range A..B | --items FILE | --inputs GLOB) [flags] -- command [args...]")
		fmt.Fprintln(stderr, "In the command, {} is replaced by the task's item and {#} by its number.")
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
	tasks, localInputs, err := buildTasks(*rangeSpec, *itemsFile, inputs, os.Stdin)
	if err != nil {
		return 2, err
	}
	shipped, err := loadShipped(ship)
	if err != nil {
		return 1, err
	}

	c := client.New()
	c.Port = *port
	devices, err := mapDevices(ctx, c, *nodes, *probeTimeout)
	if err != nil {
		return 1, err
	}

	width := len(strconv.Itoa(len(tasks)))
	taskDir := func(idx int) string { return filepath.Join(*outDir, fmt.Sprintf("%0*d", width, idx)) }
	var attemptSeq atomic.Int64
	base := proto.RunRequest{Shell: *shell, TimeoutSec: int(timeout.Seconds()), Collect: collect}
	for i := range devices {
		host := devices[i].host
		devices[i].Run = func(ctx context.Context, t fanout.Task) (fanout.Outcome, error) {
			req := base
			req.Argv = substitute(argv, t)
			req.Env = map[string]string{"POOL_TASK_INDEX": strconv.Itoa(t.Index), "POOL_TASK_ITEM": t.Item}
			for k, v := range env {
				req.Env[k] = v
			}
			req.Files = shipped
			if src, ok := localInputs[t.Index]; ok {
				data, err := os.ReadFile(src)
				if err != nil {
					return fanout.Outcome{RemoteError: err.Error(), ExitCode: 1}, nil
				}
				req.Files = append(append([]proto.InputFile(nil), shipped...), proto.InputFile{Path: t.Item, Data: data})
			}
			var so, se bytes.Buffer
			out := client.Output{Stdout: &so, Stderr: &se}
			if len(collect) > 0 {
				// Each copy of a task gets its own folder; the winner's is
				// renamed into place, so backup copies never mix files.
				out.Dir = fmt.Sprintf("%s.part-%d", taskDir(t.Index), attemptSeq.Add(1))
			}
			res, err := c.Run(ctx, host, req, out)
			if err != nil {
				if out.Dir != "" {
					os.RemoveAll(out.Dir)
				}
				return fanout.Outcome{}, err
			}
			return fanout.Outcome{
				ExitCode: res.ExitCode, Stdout: so.Bytes(), Stderr: se.Bytes(),
				RemoteError: res.RemoteError, Files: res.Files, FilesDir: out.Dir, Duration: res.Duration,
			}, nil
		}
	}

	printer := newResultPrinter(stdout, stderr, *verbose, *keepOrder)
	onResult := func(r fanout.Result) {
		if r.FilesDir != "" {
			final := taskDir(r.Index)
			os.RemoveAll(final)
			if len(r.Files) == 0 {
				os.RemoveAll(r.FilesDir)
			} else if err := os.Rename(r.FilesDir, final); err != nil {
				fmt.Fprintf(stderr, "pool: task %d: keep collected files: %v\n", r.Index, err)
			}
		}
		printer.add(r)
	}
	discard := func(o fanout.Outcome) {
		if o.FilesDir != "" {
			os.RemoveAll(o.FilesDir)
		}
	}
	start := time.Now()
	fdevs := make([]fanout.Device, len(devices))
	for i, d := range devices {
		fdevs[i] = d.Device
	}
	results := fanout.Run(ctx, tasks, fdevs, fanout.Options{
		Retries:     *retries,
		Declined:    func(err error) bool { return errors.Is(err, client.ErrBusy) },
		MaxParallel: *jobs,
		Speculate:   *speculate,
		OnResult:    onResult,
		OnDiscard:   discard,
	})
	printer.flush()
	if len(collect) > 0 {
		// Copies canceled mid-transfer can leave partial folders behind.
		leftovers, _ := filepath.Glob(filepath.Join(*outDir, "*.part-*"))
		for _, p := range leftovers {
			os.RemoveAll(p)
		}
	}

	elapsed := time.Since(start)
	ok, failed, backups := 0, 0, 0
	perDevice := map[string]int{}
	for _, r := range results {
		if r.OK() {
			ok++
		} else {
			failed++
		}
		if r.Err == nil {
			perDevice[r.Device]++
		}
		if r.Speculated {
			backups++
		}
	}
	var split []string
	for _, d := range devices {
		split = append(split, fmt.Sprintf("%s %d", d.Name, perDevice[d.Name]))
	}
	extra := ""
	if backups > 0 {
		extra = fmt.Sprintf("; %d backup copies of slow tasks", backups)
	}
	fmt.Fprintf(stderr, "pool: %d tasks in %s: %d ok, %d failed (%s%s)\n",
		len(results), elapsed.Round(10*time.Millisecond), ok, failed, strings.Join(split, ", "), extra)
	if len(collect) > 0 {
		fmt.Fprintf(stderr, "pool: collected files are in %s\n", *outDir)
	}
	if ctx.Err() != nil {
		return 130, errors.New("canceled; running tasks were stopped")
	}
	if failed > 0 {
		return 1, nil
	}
	return 0, nil
}

type mapDevice struct {
	fanout.Device
	host string
}

// mapDevices finds every device whose agent answers, optionally limited to
// the names in only. Paused devices are included: their slots wait and pick
// up work when they resume. At least one device must be accepting now.
func mapDevices(ctx context.Context, c *client.Client, only string, timeout time.Duration) ([]mapDevice, error) {
	st, err := tsutil.GetStatus(ctx)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, n := range strings.Split(only, ",") {
		if n = strings.TrimSpace(n); n != "" {
			p, err := st.Resolve(n)
			if err != nil {
				return nil, err
			}
			want[p.Name] = true
		}
	}
	var (
		results []probeResult
		devs    []mapDevice
		cands   []client.Candidate
	)
	for _, r := range probeAll(ctx, st, c, timeout) {
		if len(want) > 0 && !want[r.peer.Name] {
			continue
		}
		results = append(results, r)
		if r.err != nil {
			continue
		}
		devs = append(devs, mapDevice{Device: fanout.Device{Name: r.peer.Name, Slots: r.info.MaxJobs}, host: r.peer.IPv4})
		cands = append(cands, client.Candidate{Name: r.peer.Name, Host: r.peer.IPv4, Info: r.info})
	}
	if len(client.Rank(cands)) == 0 {
		return nil, fmt.Errorf("no device can take tasks right now (%s)", explain(results))
	}
	return devs, nil
}

// resultPrinter writes each task's output as one block, either as tasks
// finish or in input order.
type resultPrinter struct {
	stdout, stderr io.Writer
	verbose, order bool
	held           map[int]fanout.Result
	next           int
}

func newResultPrinter(stdout, stderr io.Writer, verbose, order bool) *resultPrinter {
	return &resultPrinter{stdout: stdout, stderr: stderr, verbose: verbose, order: order, held: map[int]fanout.Result{}, next: 1}
}

func (p *resultPrinter) add(r fanout.Result) {
	if !p.order {
		p.print(r)
		return
	}
	p.held[r.Index] = r
	p.flush()
}

// flush prints every held result that is next in order. Results are held
// only with --keep-order, and tasks are indexed 1..N with no gaps.
func (p *resultPrinter) flush() {
	for {
		r, ok := p.held[p.next]
		if !ok {
			return
		}
		delete(p.held, p.next)
		p.next++
		p.print(r)
	}
}

func (p *resultPrinter) print(r fanout.Result) {
	if p.verbose || !r.OK() {
		var status string
		switch {
		case r.Err != nil:
			status = "failed: " + r.Err.Error()
		case r.RemoteError != "":
			status = fmt.Sprintf("exit %d (%s)", r.ExitCode, r.RemoteError)
		default:
			status = fmt.Sprintf("exit %d in %s", r.ExitCode, r.Duration.Round(10*time.Millisecond))
		}
		where := ""
		if r.Device != "" {
			where = " on " + r.Device
		}
		fmt.Fprintf(p.stderr, "pool: task %d (%s)%s: %s\n", r.Index, r.Item, where, status)
	}
	_, _ = p.stdout.Write(r.Stdout)
	_, _ = p.stderr.Write(r.Stderr)
}
