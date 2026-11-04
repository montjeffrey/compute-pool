package report

import (
	"math"
	"sort"
	"time"

	"github.com/montjeffrey/compute-pool/internal/proto"
)

// Options tunes Analyze. Zero values pick the defaults.
type Options struct {
	// MinJobs is how many successful jobs a workload needs to be assessed
	// (default 30).
	MinJobs int
	// MinLevel is how many jobs a number-of-jobs-at-once level needs to be
	// shown, and a run needs at that level to count toward consistency
	// (default 5).
	MinLevel int
	// RunGap is how long a device must sit idle between one workload's jobs
	// to separate two runs (default 2s).
	RunGap time.Duration
}

func (o Options) withDefaults() Options {
	if o.MinJobs <= 0 {
		o.MinJobs = 30
	}
	if o.MinLevel <= 0 {
		o.MinLevel = 5
	}
	if o.RunGap <= 0 {
		o.RunGap = 2 * time.Second
	}
	return o
}

// Device is one device's log plus its pool setup measurement, if any.
type Device struct {
	Log      *Log
	Capacity *proto.Capacity
}

// Report holds the assessments for every device.
type Report struct {
	Devices []DeviceReport `json:"devices"`
}

// DeviceReport assesses one device.
type DeviceReport struct {
	Node        string           `json:"node"`
	From        time.Time        `json:"from"`
	To          time.Time        `json:"to"`
	Workloads   []WorkloadReport `json:"workloads"`
	Reliability Reliability      `json:"reliability"`
}

// WorkloadReport assesses one command run many times on one device.
type WorkloadReport struct {
	Command string `json:"command"`
	Jobs    int    `json:"jobs"` // successful jobs
	Runs    int    `json:"runs"`
	// Scaling is speed by number of jobs at once, fewest first.
	Scaling []Level `json:"scaling"`
	// CapacityKernel names the pool setup measurement Predicted comes from.
	CapacityKernel string        `json:"capacity_kernel,omitempty"`
	Consistency    []Consistency `json:"consistency"`
	SlotUse        *SlotUse      `json:"slot_use,omitempty"`
}

// Level is how fast tasks ran with Jobs jobs running at once.
type Level struct {
	Jobs   int     `json:"jobs"`
	Count  int     `json:"count"`
	Median float64 `json:"median_s"` // seconds per task
	P90    float64 `json:"p90_s"`
	// Throughput is tasks finished per second by all jobs together.
	Throughput float64 `json:"throughput"`
	// Slowdown is the median time per task relative to the lightest level
	// shown (1 there).
	Slowdown float64 `json:"slowdown"`
	// Predicted is the throughput pool setup's measurement predicts at this
	// level, scaled from the lightest level shown; 0 without a measurement.
	Predicted float64 `json:"predicted,omitempty"`
}

// Consistency is how much the time per task varied between runs at the same
// number of jobs at once.
type Consistency struct {
	Jobs   int     `json:"jobs"`
	Runs   int     `json:"runs"`
	Median float64 `json:"median_s"` // median of the runs' medians
	Low    float64 `json:"low_s"`    // 10th percentile of the runs' medians
	High   float64 `json:"high_s"`   // 90th percentile
	// Noise is the larger distance from Median to Low or High, as a
	// fraction of Median: differences smaller than this are not meaningful.
	Noise     float64         `json:"noise"`
	ByVersion []VersionMedian `json:"by_version,omitempty"`
}

// VersionMedian is the median time per task under one agent version.
type VersionMedian struct {
	Version string  `json:"version"`
	Count   int     `json:"count"`
	Median  float64 `json:"median_s"`
}

// SlotUse is how fully the device's job slots were used during runs.
type SlotUse struct {
	Runs int `json:"runs"`
	// Utilization is busy job time over slots × run length, where slots is
	// the agent's --max-jobs: time the agent lowered its live limit (other
	// programs busy, memory short) counts as unused.
	MedianUtil float64   `json:"median_util"`
	WorstUtil  float64   `json:"worst_util"`
	WorstAt    time.Time `json:"worst_at"`
	// Straggler is the slowest task's time over the run's median.
	MedianStraggler  float64   `json:"median_straggler"`
	WorstStraggler   float64   `json:"worst_straggler"`
	WorstStragglerAt time.Time `json:"worst_straggler_at"`
}

// Reliability counts how jobs ended and how often the agent was unavailable.
type Reliability struct {
	Jobs     int `json:"jobs"`
	OK       int `json:"ok"`
	Failed   int `json:"failed"`      // the program exited non-zero
	NoStart  int `json:"not_started"` // exit 127
	TimedOut int `json:"timed_out"`   // exit 124
	Canceled int `json:"canceled"`    // exit 130
	Lost     int `json:"lost"`        // the agent restarted mid-job
	Running  int `json:"running"`     // no end in the log yet
	// AgentStarts counts agent (re)starts in the log.
	AgentStarts int `json:"agent_starts"`
	// Pauses counts times the job limit fell to 0; Paused is their total
	// length, leaving out the Unended ones that lasted until the agent
	// stopped (whose length is unknown).
	Pauses  int           `json:"pauses"`
	Paused  time.Duration `json:"paused"`
	Unended int           `json:"unended_pauses"`
	// Requests refused: from devices that may not use the agent, and
	// because the caller's identity couldn't be confirmed (see Log).
	NotAllowed   int `json:"refused_not_allowed"`
	Unidentified int `json:"refused_unidentified"`
}

// Agent exit codes for jobs it ended itself (see pool run).
const (
	exitNoStart  = 127
	exitTimedOut = 124
	exitCanceled = 130
)

// Analyze assesses each device's log.
func Analyze(devs []Device, opts Options) Report {
	opts = opts.withDefaults()
	var rep Report
	for _, d := range devs {
		dr := DeviceReport{Node: d.Log.Node, From: d.Log.First, To: d.Log.Last, Reliability: reliability(d.Log)}
		byWork := map[string][]Job{}
		var order []string
		for _, j := range d.Log.Jobs {
			// A job logged as taking 0s (under half a millisecond) says
			// nothing about speed, and would divide by zero below.
			if !j.Finished() || j.Exit != 0 || j.Duration <= 0 {
				continue
			}
			if _, ok := byWork[j.Workload]; !ok {
				order = append(order, j.Workload)
			}
			byWork[j.Workload] = append(byWork[j.Workload], j)
		}
		for _, w := range order {
			if jobs := byWork[w]; len(jobs) >= opts.MinJobs {
				dr.Workloads = append(dr.Workloads, workload(w, jobs, d.Capacity, opts))
			}
		}
		sort.SliceStable(dr.Workloads, func(i, k int) bool { return dr.Workloads[i].Jobs > dr.Workloads[k].Jobs })
		rep.Devices = append(rep.Devices, dr)
	}
	return rep
}

func reliability(lg *Log) Reliability {
	r := Reliability{AgentStarts: lg.Starts, Pauses: len(lg.Pauses), NotAllowed: lg.NotAllowed, Unidentified: lg.Unidentified}
	for _, p := range lg.Pauses {
		if p.Unended {
			r.Unended++
		} else {
			r.Paused += p.To.Sub(p.From)
		}
	}
	for _, j := range lg.Jobs {
		r.Jobs++
		switch {
		case j.Lost:
			r.Lost++
		case !j.Finished():
			r.Running++
		case j.Exit == 0:
			r.OK++
		case j.Exit == exitNoStart:
			r.NoStart++
		case j.Exit == exitTimedOut:
			r.TimedOut++
		case j.Exit == exitCanceled:
			r.Canceled++
		default:
			r.Failed++
		}
	}
	return r
}

func workload(cmd string, jobs []Job, capa *proto.Capacity, opts Options) WorkloadReport {
	runs := splitRuns(jobs, opts.RunGap)
	wr := WorkloadReport{Command: cmd, Jobs: len(jobs), Runs: len(runs)}
	wr.Scaling = scaling(jobs, capa, opts)
	if len(wr.Scaling) > 0 && wr.Scaling[0].Predicted > 0 {
		wr.CapacityKernel = capa.Kernel
	}
	for _, l := range busiest(wr.Scaling, maxConsistencyLevels) {
		if c, ok := consistency(l, runs, opts); ok {
			wr.Consistency = append(wr.Consistency, c)
		}
	}
	wr.SlotUse = slotUse(runs)
	return wr
}

// maxConsistencyLevels is how many levels get a consistency check: the ones
// with the most jobs, which are the loads the device actually runs at.
const maxConsistencyLevels = 3

// busiest returns the numbers of jobs at once of the n levels with the most
// jobs, fewest jobs at once first.
func busiest(levels []Level, n int) []int {
	ls := append([]Level(nil), levels...)
	sort.SliceStable(ls, func(i, k int) bool { return ls[i].Count > ls[k].Count })
	var out []int
	for _, l := range ls[:min(n, len(ls))] {
		out = append(out, l.Jobs)
	}
	sort.Ints(out)
	return out
}

// splitRuns groups jobs into runs: jobs sent by the same device with no idle
// stretch longer than gap between them. jobs must be sorted by Start.
func splitRuns(jobs []Job, gap time.Duration) [][]Job {
	var (
		runs  [][]Job
		cur   = map[string]int{} // submitter -> index into runs
		until = map[string]time.Time{}
	)
	for _, j := range jobs {
		i, ok := cur[j.From]
		if !ok || j.Start.Sub(until[j.From]) > gap {
			runs = append(runs, nil)
			i = len(runs) - 1
			cur[j.From] = i
		}
		runs[i] = append(runs[i], j)
		if j.End.After(until[j.From]) {
			until[j.From] = j.End
		}
	}
	return runs
}

func scaling(jobs []Job, capa *proto.Capacity, opts Options) []Level {
	by := map[int][]float64{}
	for _, j := range jobs {
		by[j.Concurrent] = append(by[j.Concurrent], j.Duration.Seconds())
	}
	var out []Level
	for c, xs := range by {
		if len(xs) < opts.MinLevel {
			continue
		}
		sort.Float64s(xs)
		med := percentile(xs, 0.5)
		out = append(out, Level{Jobs: c, Count: len(xs), Median: med, P90: percentile(xs, 0.9), Throughput: float64(c) / med})
	}
	sort.Slice(out, func(i, k int) bool { return out[i].Jobs < out[k].Jobs })
	if len(out) == 0 {
		return out
	}
	base := out[0]
	baseRate := capa.RateAt(base.Jobs) // 0 when not measured (nil-safe)
	for i := range out {
		out[i].Slowdown = out[i].Median / base.Median
		if baseRate > 0 {
			out[i].Predicted = base.Throughput * capa.RateAt(out[i].Jobs) / baseRate
		}
	}
	return out
}

func consistency(level int, runs [][]Job, opts Options) (Consistency, bool) {
	var meds []float64
	byVer := map[string][]float64{}
	for _, run := range runs {
		var xs []float64
		var vers []string
		for _, j := range run {
			if j.Concurrent == level {
				xs = append(xs, j.Duration.Seconds())
				vers = append(vers, j.Version)
			}
		}
		if len(xs) < opts.MinLevel {
			continue
		}
		for i, v := range vers {
			if v == "" {
				v = "unknown" // the agent's start line was rotated away
			}
			byVer[v] = append(byVer[v], xs[i])
		}
		sort.Float64s(xs)
		meds = append(meds, percentile(xs, 0.5))
	}
	if len(meds) < 3 {
		return Consistency{}, false
	}
	sort.Float64s(meds)
	c := Consistency{Jobs: level, Runs: len(meds), Median: percentile(meds, 0.5), Low: percentile(meds, 0.1), High: percentile(meds, 0.9)}
	c.Noise = max(c.High-c.Median, c.Median-c.Low) / c.Median
	for v, xs := range byVer {
		if len(xs) >= 4*opts.MinLevel {
			sort.Float64s(xs)
			c.ByVersion = append(c.ByVersion, VersionMedian{Version: v, Count: len(xs), Median: percentile(xs, 0.5)})
		}
	}
	if len(c.ByVersion) < 2 {
		c.ByVersion = nil // nothing to compare
	}
	sort.Slice(c.ByVersion, func(i, k int) bool { return c.ByVersion[i].Version < c.ByVersion[k].Version })
	return c, true
}

// slotUse looks at runs with at least twice as many tasks as job slots: in
// shorter ones, slots left idle at the end are unavoidable.
func slotUse(runs [][]Job) *SlotUse {
	var utils, strag []float64
	su := SlotUse{WorstUtil: 2}
	for _, run := range runs {
		slots := run[0].MaxJobs
		if slots <= 0 || len(run) < 2*slots {
			continue
		}
		start, end := run[0].Start, run[0].End
		var busy float64
		durs := make([]float64, 0, len(run))
		for _, j := range run {
			if j.End.After(end) {
				end = j.End
			}
			busy += j.Duration.Seconds()
			durs = append(durs, j.Duration.Seconds())
		}
		span := end.Sub(start).Seconds()
		if span <= 0 {
			continue
		}
		sort.Float64s(durs)
		u := busy / (float64(slots) * span)
		s := durs[len(durs)-1] / percentile(durs, 0.5)
		utils, strag = append(utils, u), append(strag, s)
		if u < su.WorstUtil {
			su.WorstUtil, su.WorstAt = u, start
		}
		if s > su.WorstStraggler {
			su.WorstStraggler, su.WorstStragglerAt = s, start
		}
	}
	if len(utils) == 0 {
		return nil
	}
	sort.Float64s(utils)
	sort.Float64s(strag)
	su.Runs, su.MedianUtil, su.MedianStraggler = len(utils), percentile(utils, 0.5), percentile(strag, 0.5)
	return &su
}

// percentile returns the nearest-rank p-quantile of sorted xs: the smallest
// value with at least p of the values at or below it.
func percentile(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	i := int(math.Ceil(p*float64(len(xs)))) - 1
	return xs[min(max(i, 0), len(xs)-1)]
}
