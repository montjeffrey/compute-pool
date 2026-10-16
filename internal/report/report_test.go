package report

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/montjeffrey/compute-pool/internal/proto"
)

// logWriter builds a synthetic agent log in the format the agent writes.
type logWriter struct {
	lines []entry
	seq   int
}

type entry struct {
	at   time.Time
	text string
}

var t0 = time.Date(2026, 10, 1, 9, 0, 0, 0, time.Local)

func (w *logWriter) add(at time.Time, format string, args ...any) {
	w.lines = append(w.lines, entry{at, fmt.Sprintf(format, args...)})
}

func (w *logWriter) agentStart(at time.Time, version string, maxJobs int) {
	w.add(at, "pool agent %s on node-a; owner someone@example; allowed devices: node-a, node-b; max jobs %d", version, maxJobs)
}

// job logs a job from node-b running argv from start for d, ending with code.
func (w *logWriter) job(start time.Time, d time.Duration, code int, argv ...string) {
	w.seq++
	id := fmt.Sprintf("node-a-%s-%d", start.Format("20060102T150405"), w.seq)
	w.add(start, "job %s from node-b: %q (shell=false)", id, argv)
	w.add(start.Add(d), "job %s done: exit=%d in %s", id, code, d.Round(time.Millisecond))
}

// render writes the log in time order with the given timestamp layout.
func (w *logWriter) render(layout string) string {
	sort.SliceStable(w.lines, func(i, k int) bool { return w.lines[i].at.Before(w.lines[k].at) })
	var b strings.Builder
	for _, l := range w.lines {
		fmt.Fprintf(&b, "%s %s\n", l.at.Format(layout), l.text)
	}
	return b.String()
}

const (
	precise = "2006/01/02 15:04:05.000000"
	coarse  = "2006/01/02 15:04:05"
)

func mustParse(t *testing.T, s string) *Log {
	t.Helper()
	lg, err := Parse(strings.NewReader(s))
	if err != nil {
		t.Fatal(err)
	}
	return lg
}

// batch runs tasks one after another in each of slots slots, all taking d,
// from start; it returns when the last one ends.
func (w *logWriter) batch(start time.Time, tasks, slots int, d time.Duration, argv func(task int) []string) time.Time {
	end := start
	for i := 0; i < tasks; i++ {
		s := start.Add(time.Duration(i/slots) * d)
		w.job(s, d, 0, argv(i+1)...)
		if e := s.Add(d); e.After(end) {
			end = e
		}
	}
	return end
}

func bench(n string) func(int) []string {
	return func(task int) []string { return []string{"bench.exe", "-n", n, fmt.Sprint(task)} }
}

func TestParseLines(t *testing.T) {
	var w logWriter
	w.agentStart(t0, "0.1.4", 4)
	w.job(t0.Add(time.Second), 1500*time.Millisecond, 0, "prog", "two words", `quote"d`)
	w.job(t0.Add(2*time.Second), 300*time.Millisecond, 3, "prog")
	w.add(t0.Add(3*time.Second), "refused GET /v1/run from 100.64.0.9: not allowed")
	w.add(t0.Add(3*time.Second), `refused GET "/v1/info" from 100.83.94.81: identify 100.83.94.81: tailscale whois --json 100.83.94.81: context canceled: `)
	w.add(t0.Add(3*time.Second), `refused POST "/v1/run" from 100.64.0.3: forbidden: node sales-box is not on the allow list`)
	w.add(t0.Add(4*time.Second), "job limit now 0: 0.5 GB of memory free")
	w.add(t0.Add(64*time.Second), "job limit now 2")
	w.add(t0.Add(65*time.Second), "job node-a-x-99 done: exit=0 in 1s") // start rotated away
	w.add(t0.Add(70*time.Second), "job node-a-lost-1 from node-b: [\"slow\"] (shell=true)")
	w.add(t0.Add(75*time.Second), "job limit now 0: CPU busy")
	w.agentStart(t0.Add(80*time.Second), "0.1.5", 8)
	w.add(t0.Add(81*time.Second), "not a log line")

	lg := mustParse(t, w.render(precise)+"garbage\n")
	if lg.Node != "node-a" || lg.Starts != 2 || lg.NotAllowed != 2 || lg.Unidentified != 1 {
		t.Errorf("node=%q starts=%d not allowed=%d unidentified=%d", lg.Node, lg.Starts, lg.NotAllowed, lg.Unidentified)
	}
	if !lg.First.Equal(t0) || !lg.Last.Equal(t0.Add(81*time.Second)) {
		t.Errorf("first=%v last=%v", lg.First, lg.Last)
	}
	if len(lg.Jobs) != 3 {
		t.Fatalf("got %d jobs, want 3: %+v", len(lg.Jobs), lg.Jobs)
	}
	j := lg.Jobs[0]
	if got := strings.Join(j.Argv, "|"); got != `prog|two words|quote"d` {
		t.Errorf("argv = %q", got)
	}
	if j.Duration != 1500*time.Millisecond || !j.Start.Equal(t0.Add(time.Second)) || j.Version != "0.1.4" || j.MaxJobs != 4 || j.From != "node-b" {
		t.Errorf("job = %+v", j)
	}
	if lg.Jobs[1].Exit != 3 {
		t.Errorf("exit = %d, want 3", lg.Jobs[1].Exit)
	}
	if lost := lg.Jobs[2]; !lost.Lost || !lost.Shell || lost.Finished() {
		t.Errorf("want the job open at the restart to be lost: %+v", lost)
	}
	if len(lg.Pauses) != 2 {
		t.Fatalf("pauses = %+v", lg.Pauses)
	}
	if p := lg.Pauses[0]; p.Unended || p.To.Sub(p.From) != time.Minute || p.Reason != "0.5 GB of memory free" {
		t.Errorf("first pause = %+v", p)
	}
	// The second pause ended with the agent: its end is the last line seen
	// before the restart, not the restart itself.
	if p := lg.Pauses[1]; !p.Unended || !p.To.Equal(t0.Add(75*time.Second)) {
		t.Errorf("second pause = %+v", p)
	}
}

func TestParseOpenJobAtEndIsRunning(t *testing.T) {
	var w logWriter
	w.agentStart(t0, "0.1.5", 2)
	w.add(t0.Add(time.Second), `job node-a-1 from node-b: ["x"] (shell=false)`)
	lg := mustParse(t, w.render(precise))
	rel := Analyze([]Device{{Log: lg}}, Options{}).Devices[0].Reliability
	if rel.Running != 1 || rel.Lost != 0 {
		t.Errorf("reliability = %+v", rel)
	}
}

func TestEndTimeNarrowsWholeSecondTimestamps(t *testing.T) {
	// A job really ran 10:00:00.700 → 10:00:02.500 (1.8s); the log shows
	// 10:00:00 and 10:00:02. The end must be in [02.0, 03.0) and in
	// [00.0+1.8, 01.0+1.8) = [01.8, 02.8): the middle of [02.0, 02.8) is 02.4.
	base := time.Date(2026, 10, 1, 10, 0, 0, 0, time.Local)
	got := endTime(started{at: base}, base.Add(2*time.Second), false, 1800*time.Millisecond)
	if want := base.Add(2400 * time.Millisecond); !got.Equal(want) {
		t.Errorf("end = %v, want %v", got.Format(precise), want.Format(precise))
	}
	// Precise timestamps are used as they are.
	exact := base.Add(2500 * time.Millisecond)
	if got := endTime(started{at: base, precise: true}, exact, true, 1800*time.Millisecond); !got.Equal(exact) {
		t.Errorf("precise end = %v", got)
	}
}

func TestConcurrency(t *testing.T) {
	var w logWriter
	w.agentStart(t0, "0.1.5", 4)
	s := t0.Add(time.Second)
	// a and b side by side for 2s; c starts as they end; d overlaps c by 1s.
	w.job(s, 2*time.Second, 0, "a")
	w.job(s, 2*time.Second, 0, "b")
	w.job(s.Add(2*time.Second), 2*time.Second, 0, "c")
	w.job(s.Add(3*time.Second), 4*time.Second, 0, "d")
	lg := mustParse(t, w.render(precise))
	got := map[string]int{}
	for _, j := range lg.Jobs {
		got[j.Argv[0]] = j.Concurrent
	}
	// c: 1s alone, 1s with d → 1.5 → 2. d: 1s with c, 3s alone → 1.25 → 1.
	want := map[string]int{"a": 2, "b": 2, "c": 2, "d": 1}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s concurrent = %d, want %d (all: %v)", k, got[k], v, got)
		}
	}
}

func TestConcurrencyCappedAtMaxJobs(t *testing.T) {
	var w logWriter
	w.agentStart(t0, "0.1.4", 2)
	for i := 0; i < 4; i++ { // the log claims 4 overlapping jobs on a 2-slot agent
		w.job(t0.Add(time.Second), time.Second, 0, "x")
	}
	for _, j := range mustParse(t, w.render(precise)).Jobs {
		if j.Concurrent != 2 {
			t.Errorf("concurrent = %d, want 2", j.Concurrent)
		}
	}
}

func TestWorkloads(t *testing.T) {
	var w logWriter
	w.agentStart(t0, "0.1.5", 4)
	end := w.batch(t0.Add(time.Second), 40, 4, time.Second, bench("8000"))
	end = w.batch(end.Add(5*time.Second), 40, 4, time.Second, bench("4000"))
	w.job(end.Add(time.Second), time.Second, 0, "bench.exe", "-n", "8000", "1")
	w.job(end.Add(2*time.Second), time.Second, 0, "cmd /c echo hi")
	counts := map[string]int{}
	for _, j := range mustParse(t, w.render(precise)).Jobs {
		counts[j.Workload]++
	}
	want := map[string]int{"bench.exe -n 8000 {}": 41, "bench.exe -n 4000 {}": 40, `"cmd /c echo hi"`: 1}
	if fmt.Sprint(counts) != fmt.Sprint(want) {
		t.Errorf("workloads = %v, want %v", counts, want)
	}
}

func TestWorkloadFewTasksKeepsArguments(t *testing.T) {
	// Only a handful of distinct task numbers: too few to call the argument
	// the task, so each stays its own (too small to assess) workload.
	var w logWriter
	w.agentStart(t0, "0.1.5", 2)
	w.batch(t0.Add(time.Second), 6, 2, time.Second, bench("8000"))
	for _, j := range mustParse(t, w.render(precise)).Jobs {
		if strings.Contains(j.Workload, "{}") {
			t.Errorf("workload %q should keep its task argument", j.Workload)
		}
	}
}

// steadyLog is a warm-up run of 8 tasks one at a time, then three runs of 40
// one-second tasks on a 4-slot agent, then four jobs that end badly.
func steadyLog(t *testing.T, layout string) *Log {
	var w logWriter
	w.agentStart(t0, "0.1.4", 4)
	end := w.batch(t0.Add(time.Second), 8, 1, time.Second, bench("8000"))
	for r := 0; r < 3; r++ {
		end = w.batch(end.Add(10*time.Second), 40, 4, time.Second, bench("8000"))
	}
	w.job(end.Add(time.Second), time.Second, 1, "bench.exe", "-n", "8000", "x")
	w.job(end.Add(2*time.Second), time.Second, 130, "other")
	w.job(end.Add(3*time.Second), time.Second, 124, "other")
	w.job(end.Add(4*time.Second), time.Second, 127, "other")
	return mustParse(t, w.render(layout))
}

func TestAnalyzeSteadyRuns(t *testing.T) {
	// Linear scaling in the setup measurement: 4 jobs are 4× one job.
	capa := &proto.Capacity{Kernel: "k1", Levels: []proto.Level{{Jobs: 1, Rate: 10}, {Jobs: 4, Rate: 40}}}
	rep := Analyze([]Device{{Log: steadyLog(t, precise), Capacity: capa}}, Options{})
	if len(rep.Devices) != 1 || len(rep.Devices[0].Workloads) != 1 {
		t.Fatalf("report = %+v", rep)
	}
	wr := rep.Devices[0].Workloads[0]
	if wr.Command != "bench.exe -n 8000 {}" || wr.Jobs != 128 || wr.Runs != 4 {
		t.Errorf("workload %q: %d jobs in %d runs", wr.Command, wr.Jobs, wr.Runs)
	}
	if len(wr.Scaling) != 2 {
		t.Fatalf("scaling = %+v", wr.Scaling)
	}
	one, four := wr.Scaling[0], wr.Scaling[1]
	if one.Jobs != 1 || one.Count != 8 || one.Median != 1 || one.Throughput != 1 || one.Slowdown != 1 {
		t.Errorf("1 job level = %+v", one)
	}
	if four.Jobs != 4 || four.Count != 120 || four.Throughput != 4 || four.Predicted != 4 {
		t.Errorf("4 jobs level = %+v", four)
	}
	if wr.CapacityKernel != "k1" {
		t.Errorf("kernel = %q", wr.CapacityKernel)
	}
	// Only the 4-jobs level has 3 runs; the warm-up is a single run.
	if len(wr.Consistency) != 1 {
		t.Fatalf("consistency = %+v", wr.Consistency)
	}
	if c := wr.Consistency[0]; c.Jobs != 4 || c.Runs != 3 || c.Noise != 0 || c.Median != 1 {
		t.Errorf("consistency at 4 = %+v", c)
	}
	// The warm-up is one job at a time on 4 slots: 2× slots but 25% used.
	su := wr.SlotUse
	if su == nil || su.Runs != 4 || math.Abs(su.MedianUtil-1) > 1e-9 || math.Abs(su.WorstUtil-0.25) > 1e-9 || su.MedianStraggler != 1 {
		t.Errorf("slot use = %+v", su)
	}
	rel := rep.Devices[0].Reliability
	want := Reliability{Jobs: 132, OK: 128, Failed: 1, Canceled: 1, TimedOut: 1, NoStart: 1, AgentStarts: 1}
	if rel != want {
		t.Errorf("reliability = %+v, want %+v", rel, want)
	}
}

func TestAnalyzeWholeSecondLog(t *testing.T) {
	// Agents before 0.1.5 log whole seconds; the levels must still come out.
	rep := Analyze([]Device{{Log: steadyLog(t, coarse)}}, Options{})
	wr := rep.Devices[0].Workloads[0]
	byJobs := map[int]Level{}
	for _, l := range wr.Scaling {
		byJobs[l.Jobs] = l
	}
	if byJobs[4].Count < 100 || byJobs[1].Count < 5 {
		t.Errorf("scaling = %+v", wr.Scaling)
	}
	if wr.Scaling[0].Predicted != 0 || wr.CapacityKernel != "" {
		t.Error("no measurement: nothing should be predicted")
	}
}

func TestSlotUseStraggler(t *testing.T) {
	s := t0
	job := func(start, d float64) Job {
		st := s.Add(time.Duration(start * float64(time.Second)))
		return Job{From: "b", MaxJobs: 2, Start: st, End: st.Add(time.Duration(d * float64(time.Second))), Duration: time.Duration(d * float64(time.Second))}
	}
	// Two slots: 1s tasks at 0 and 1 in both, then a 3s straggler at 2.
	jobs := []Job{job(0, 1), job(0, 1), job(1, 1), job(1, 1), job(2, 3)}
	su := slotUse(splitRuns(jobs, 2*time.Second))
	// 7s of work over 2 slots × 5s; slowest 3s against a median of 1s.
	if su == nil || math.Abs(su.MedianUtil-0.7) > 1e-9 || su.WorstStraggler != 3 {
		t.Errorf("slot use = %+v", su)
	}
}

func TestSplitRunsBySubmitterAndGap(t *testing.T) {
	jobs := []Job{
		{From: "a", Start: t0, End: t0.Add(time.Second)},
		{From: "b", Start: t0, End: t0.Add(time.Second)},
		{From: "a", Start: t0.Add(2 * time.Second), End: t0.Add(3 * time.Second)},   // 1s idle: same run
		{From: "a", Start: t0.Add(10 * time.Second), End: t0.Add(11 * time.Second)}, // 7s idle: new run
	}
	var sizes []int
	for _, r := range splitRuns(jobs, 2*time.Second) {
		sizes = append(sizes, len(r))
	}
	if fmt.Sprint(sizes) != "[2 1 1]" {
		t.Errorf("run sizes = %v", sizes)
	}
}

func TestConsistencyNoise(t *testing.T) {
	// Five runs whose medians are 1.0, 1.0, 1.1, 1.2, 1.5 s.
	var runs [][]Job
	for _, sec := range []float64{1.0, 1.0, 1.1, 1.2, 1.5} {
		var run []Job
		for i := 0; i < 5; i++ {
			run = append(run, Job{Concurrent: 2, Version: "v", Duration: time.Duration(sec * float64(time.Second))})
		}
		runs = append(runs, run)
	}
	c, ok := consistency(2, runs, Options{}.withDefaults())
	if !ok {
		t.Fatal("no consistency")
	}
	// Median 1.1; 10th/90th percentiles 1.0 and 1.5: noise = 0.4/1.1.
	if c.Runs != 5 || c.Median != 1.1 || c.Low != 1.0 || c.High != 1.5 || math.Abs(c.Noise-0.4/1.1) > 1e-9 {
		t.Errorf("consistency = %+v", c)
	}
	if c.ByVersion != nil {
		t.Errorf("one version: nothing to compare, got %+v", c.ByVersion)
	}
	if _, ok := consistency(2, runs[:2], Options{}.withDefaults()); ok {
		t.Error("two runs are too few")
	}
}

func TestConsistencyByVersion(t *testing.T) {
	var runs [][]Job
	for i, v := range []string{"0.1.4", "0.1.4", "0.1.5", "0.1.5"} {
		var run []Job
		for k := 0; k < 20; k++ {
			run = append(run, Job{Concurrent: 2, Version: v, Duration: time.Duration(1+i/2) * time.Second})
		}
		runs = append(runs, run)
	}
	c, _ := consistency(2, runs, Options{}.withDefaults())
	if fmt.Sprint(c.ByVersion) != "[{0.1.4 40 1} {0.1.5 40 2}]" {
		t.Errorf("by version = %v", c.ByVersion)
	}
}

func TestBusiest(t *testing.T) {
	levels := []Level{{Jobs: 1, Count: 5}, {Jobs: 4, Count: 90}, {Jobs: 6, Count: 7}, {Jobs: 8, Count: 50}, {Jobs: 9, Count: 6}}
	if got := fmt.Sprint(busiest(levels, 3)); got != "[4 6 8]" {
		t.Errorf("busiest = %s", got)
	}
}

func TestWriteText(t *testing.T) {
	capa := &proto.Capacity{Kernel: "k1", Levels: []proto.Level{{Jobs: 1, Rate: 10}, {Jobs: 4, Rate: 36}}}
	lg := steadyLog(t, precise)
	lg.Pauses = []Pause{{From: t0, To: t0.Add(time.Minute)}, {From: t0, To: t0, Unended: true}}
	var b bytes.Buffer
	WriteText(&b, Analyze([]Device{{Log: lg, Capacity: capa}}, Options{}))
	out := b.String()
	for _, want := range []string{
		"=== node-a: log from 2026-10-01 09:00",
		"--- bench.exe -n 8000 {}  (128 successful jobs in 4 runs)",
		"1. Speed by jobs at once", "SETUP PREDICTS", "3.60/s (+11%)",
		"1→4 jobs at once: 4.0× the slots gave 4.00× the throughput; each task took 0% longer.",
		"2. Consistency between runs", "4 jobs at once, 3 runs: median 1.00s per task",
		"3. Slot use", "4 runs: slots busy 100% of the run (median), lowest 25%",
		"4. Reliability (all jobs)", "132 jobs: 128 (97.0%) succeeded, 1 (0.8%) failed in the program",
		"took no jobs 1 times (job limit 0) for 1m0s in all", "was taking no jobs when it stopped 1 times",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestRefusedUnidentified(t *testing.T) {
	for _, tt := range []struct {
		line string
		want bool
	}{
		{`refused GET "/v1/info" from 100.83.94.81: identify 100.83.94.81: tailscale whois --json 100.83.94.81: context canceled: `, true},
		{`refused GET "/v1/info" from 100.83.94.81: identify 100.83.94.81: tailscale whois --json 100.83.94.81: exit status 1: `, true},
		{`refused GET "/v1/info" from 100.83.94.81: identify 100.83.94.81: tailscale whois --json 100.83.94.81: exec: canceling Cmd: TerminateProcess: Access is denied.: `, true},
		{`refused GET /v1/info from 100.83.94.81: identify 100.83.94.81: context deadline exceeded`, true}, // agents before 0.1.5
		{`refused GET "/v1/info" from 100.64.0.4: forbidden: laptop belongs to mallory@github, not alice@github`, false},
		{`refused GET "/v1/info" from 100.64.0.3: forbidden: node sales-box is not on the allow list`, false},
		{`refused GET "/v1/info" from 192.168.1.10: forbidden: 192.168.1.10 is not a Tailscale address`, false},
		{`refused POST "/v1/run" from 100.64.0.1: browser-originated request or unexpected Host "evil.example"`, false},
		// The path is the caller's: text in it must not change the verdict.
		{`refused GET "/x from 100.64.0.1: identify 100.64.0.1: y" from 100.64.0.3: forbidden: node sales-box is not on the allow list`, false},
		{`refused GET "/v1/info" from 100.64.0.1: browser-originated request or unexpected Host "a: identify b"`, false},
		{`refused`, false},
	} {
		if got := refusedUnidentified(tt.line); got != tt.want {
			t.Errorf("refusedUnidentified(%s) = %v, want %v", tt.line, got, tt.want)
		}
	}
}

func TestReliabilitySeparatesRefusals(t *testing.T) {
	var w logWriter
	w.agentStart(t0, "0.1.5", 2)
	for i := 0; i < 3; i++ {
		w.add(t0.Add(time.Duration(i+1)*time.Second), `refused GET "/v1/info" from 100.83.94.81: identify 100.83.94.81: tailscale whois --json 100.83.94.81: context canceled: `)
	}
	w.add(t0.Add(5*time.Second), `refused GET "/v1/info" from 100.64.0.4: forbidden: laptop belongs to mallory@github, not alice@github`)
	rep := Analyze([]Device{{Log: mustParse(t, w.render(precise))}}, Options{})
	rel := rep.Devices[0].Reliability
	if rel.Unidentified != 3 || rel.NotAllowed != 1 {
		t.Errorf("unidentified=%d not allowed=%d, want 3 and 1", rel.Unidentified, rel.NotAllowed)
	}

	var b bytes.Buffer
	WriteText(&b, rep)
	for _, want := range []string{
		"refused 3 requests because it couldn't confirm the caller's identity",
		"refused 1 requests from devices that aren't allowed",
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, b.String())
		}
	}

	js, err := json.Marshal(rel)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"refused_not_allowed":1`, `"refused_unidentified":3`} {
		if !strings.Contains(string(js), want) {
			t.Errorf("JSON lacks %s: %s", want, js)
		}
	}
}

func TestWriteTextNothingToAssess(t *testing.T) {
	var w logWriter
	w.agentStart(t0, "0.1.5", 2)
	w.job(t0.Add(time.Second), time.Second, 0, "once")
	var b bytes.Buffer
	WriteText(&b, Analyze([]Device{{Log: mustParse(t, w.render(precise))}}, Options{}))
	if !strings.Contains(b.String(), "No command ran often enough") {
		t.Errorf("output:\n%s", b.String())
	}
}

func TestParseSkipsOverlongLines(t *testing.T) {
	var w logWriter
	w.agentStart(t0, "0.1.5", 2)
	w.job(t0.Add(time.Second), time.Second, 0, "prog", strings.Repeat("a", maxLineBytes+10))
	w.job(t0.Add(3*time.Second), time.Second, 0, "after")
	lg := mustParse(t, w.render(precise))
	// The huge start line is dropped (so its done line has no job); the
	// rest of the log is still read.
	if len(lg.Jobs) != 1 || lg.Jobs[0].Argv[0] != "after" {
		t.Errorf("jobs = %+v", lg.Jobs)
	}
}

func TestParseLastLineWithoutNewline(t *testing.T) {
	var w logWriter
	w.agentStart(t0, "0.1.5", 2)
	w.job(t0.Add(time.Second), time.Second, 0, "x")
	lg := mustParse(t, strings.TrimSuffix(w.render(precise), "\n"))
	if len(lg.Jobs) != 1 || !lg.Jobs[0].Finished() {
		t.Errorf("jobs = %+v", lg.Jobs)
	}
}

func TestZeroDurationJobsDontBreakTheReport(t *testing.T) {
	var w logWriter
	w.agentStart(t0, "0.1.5", 4)
	w.batch(t0.Add(time.Second), 40, 4, 0, bench("1")) // all logged as "0s"
	rep := Analyze([]Device{{Log: mustParse(t, w.render(precise))}}, Options{})
	if len(rep.Devices[0].Workloads) != 0 {
		t.Errorf("0s jobs can't be assessed: %+v", rep.Devices[0].Workloads)
	}
	if rep.Devices[0].Reliability.OK != 40 {
		t.Errorf("they still count as succeeded: %+v", rep.Devices[0].Reliability)
	}
}

func TestPercentile(t *testing.T) {
	xs := []float64{1, 2, 3, 4}
	for _, c := range []struct{ p, want float64 }{{0.1, 1}, {0.25, 1}, {0.5, 2}, {0.51, 3}, {0.9, 4}, {1, 4}} {
		if got := percentile(xs, c.p); got != c.want {
			t.Errorf("percentile(%v) = %v, want %v", c.p, got, c.want)
		}
	}
	if percentile(nil, 0.5) != 0 {
		t.Error("empty: want 0")
	}
}

func TestWorkloadKeepsOneSpellingOfTheProgram(t *testing.T) {
	var w logWriter
	w.agentStart(t0, "0.1.5", 4)
	w.batch(t0.Add(time.Second), 20, 4, time.Second, bench("8"))
	w.batch(t0.Add(time.Minute), 20, 4, time.Second, func(task int) []string { return []string{"BENCH.exe", "-n", "8", fmt.Sprint(100 + task)} })
	names := map[string]bool{}
	for _, j := range mustParse(t, w.render(precise)).Jobs {
		names[j.Workload] = true
	}
	if len(names) != 1 {
		t.Errorf("workloads = %v, want one", names)
	}
}

func TestConsistencyUnknownVersion(t *testing.T) {
	var runs [][]Job
	for i, v := range []string{"", "", "0.1.5", "0.1.5"} {
		var run []Job
		for k := 0; k < 20; k++ {
			run = append(run, Job{Concurrent: 2, Version: v, Duration: time.Duration(1+i/2) * time.Second})
		}
		runs = append(runs, run)
	}
	// A run below MinLevel at this level adds nothing to the version counts.
	runs = append(runs, []Job{{Concurrent: 2, Version: "0.1.5", Duration: 9 * time.Second}})
	c, _ := consistency(2, runs, Options{}.withDefaults())
	if fmt.Sprint(c.ByVersion) != "[{0.1.5 40 2} {unknown 40 1}]" {
		t.Errorf("by version = %v", c.ByVersion)
	}
}

func TestWriteTextEmptyLog(t *testing.T) {
	var b bytes.Buffer
	WriteText(&b, Analyze([]Device{{Log: mustParse(t, "")}}, Options{}))
	if !strings.Contains(b.String(), "the log is empty") || strings.Contains(b.String(), "0001") {
		t.Errorf("output:\n%s", b.String())
	}
}
