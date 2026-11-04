// Package report turns agent logs into assessments of how the pool performed
// in real use: speed by number of jobs at once, how much results vary between
// runs, how fully job slots were used, and how reliably jobs finished.
package report

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Job is one job from an agent log.
type Job struct {
	ID    string   `json:"id"`
	From  string   `json:"from"`
	Argv  []string `json:"argv"`
	Shell bool     `json:"shell,omitempty"`
	// Start is End minus Duration for a finished job, which is more precise
	// than the start line's timestamp; a lost job keeps the start line's.
	Start    time.Time     `json:"start"`
	End      time.Time     `json:"end"`
	Duration time.Duration `json:"duration"`
	Exit     int           `json:"exit"`
	// Lost is set when the agent restarted before the job finished.
	Lost bool `json:"lost,omitempty"`
	// Version and MaxJobs describe the agent that ran the job.
	Version string `json:"version"`
	MaxJobs int    `json:"max_jobs"`
	// Concurrent is the average number of jobs running on the device while
	// this one ran (itself included), rounded and at most MaxJobs.
	Concurrent int `json:"concurrent"`
	// Workload groups jobs running the same command, with the argument that
	// changes from task to task shown as {}.
	Workload string `json:"workload"`
}

// started is when the start line was logged and whether its timestamp has
// sub-second precision (agents before 0.1.5 log whole seconds).
type started struct {
	at      time.Time
	precise bool
}

// Finished reports whether the job ran to completion (any exit code).
func (j Job) Finished() bool { return !j.Lost && !j.End.IsZero() }

// Pause is a period when the agent's job limit was 0 (memory short or the
// device busy), so it took no work.
type Pause struct {
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
	Reason string    `json:"reason,omitempty"`
	// Unended is set when the agent stopped (or the log ends) while paused:
	// the device may have been off, so To is only the last line seen.
	Unended bool `json:"unended,omitempty"`
}

// Log is what one agent log says.
type Log struct {
	Node   string    `json:"node"`
	First  time.Time `json:"first"`
	Last   time.Time `json:"last"`
	Jobs   []Job     `json:"-"`
	Starts int       `json:"agent_starts"` // times the agent started
	Pauses []Pause   `json:"pauses"`
	// Requests refused by authentication: NotAllowed came from a device
	// (or a web page) that may not use the agent; Unidentified were refused
	// because the agent couldn't confirm who was calling (tailscale whois
	// failed or was too slow), which is usually the owner's own device
	// asking while one of the two is busy.
	NotAllowed   int `json:"refused_not_allowed"`
	Unidentified int `json:"refused_unidentified"`
}

var (
	reAgent = regexp.MustCompile(`^pool agent (\S+) on (\S+); .*max jobs (\d+)`)
	reStart = regexp.MustCompile(`^job (\S+) from (\S+): (\[.*\]) \(shell=(true|false)\)$`)
	reDone  = regexp.MustCompile(`^job (\S+) done: exit=(-?\d+) in (\S+)$`)
	reLimit = regexp.MustCompile(`^job limit now (\d+)(?:: (.*))?$`)
)

// Parse reads an agent log (older lines first, as written). Lines it doesn't
// recognize are skipped, so logs from any agent version can be read.
func Parse(r io.Reader) (*Log, error) {
	lg := &Log{}
	var (
		version string
		maxJobs int
		open    = map[string]*Job{} // started, not yet done
		begun   = map[string]started{}
		paused  *Pause
		prev    time.Time // the previous line's time
	)
	// closeSession ends an agent run that stopped after the line at last.
	closeSession := func(last time.Time) {
		for id, j := range open {
			j.Lost = true
			lg.Jobs = append(lg.Jobs, *j)
			delete(open, id)
			delete(begun, id)
		}
		if paused != nil {
			paused.To, paused.Unended = last, true
			lg.Pauses = append(lg.Pauses, *paused)
			paused = nil
		}
	}

	lines := newLineReader(r)
	for {
		line, err := lines.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		at, precise, msg, ok := splitLine(line)
		if !ok {
			continue
		}
		if lg.First.IsZero() {
			lg.First = at
		}
		lg.Last = at
		last := prev
		prev = at
		if m := reAgent.FindStringSubmatch(msg); m != nil {
			closeSession(last)
			version, lg.Node = m[1], m[2]
			maxJobs, _ = strconv.Atoi(m[3])
			lg.Starts++
			continue
		}
		if m := reStart.FindStringSubmatch(msg); m != nil {
			argv, err := parseArgv(m[3])
			if err != nil {
				continue
			}
			// Start is refined from the duration once the job is done.
			open[m[1]] = &Job{ID: m[1], From: m[2], Argv: argv, Shell: m[4] == "true", Start: at, Version: version, MaxJobs: maxJobs}
			begun[m[1]] = started{at, precise}
			continue
		}
		if m := reDone.FindStringSubmatch(msg); m != nil {
			j, ok := open[m[1]]
			if !ok {
				continue // its start was in an older, rotated-away log
			}
			d, err := time.ParseDuration(m[3])
			if err != nil {
				continue
			}
			j.Exit, _ = strconv.Atoi(m[2])
			j.End, j.Duration = endTime(begun[m[1]], at, precise, d), d
			j.Start = j.End.Add(-d)
			delete(begun, m[1])
			lg.Jobs = append(lg.Jobs, *j)
			delete(open, m[1])
			continue
		}
		if m := reLimit.FindStringSubmatch(msg); m != nil {
			if m[1] == "0" {
				if paused == nil {
					paused = &Pause{From: at, Reason: m[2]}
				}
			} else if paused != nil {
				paused.To = at
				lg.Pauses = append(lg.Pauses, *paused)
				paused = nil
			}
			continue
		}
		if strings.HasPrefix(msg, "refused ") {
			if refusedUnidentified(msg) {
				lg.Unidentified++
			} else {
				lg.NotAllowed++
			}
		}
	}
	// Jobs still open at the end may be running now; they are not lost.
	for _, j := range open {
		lg.Jobs = append(lg.Jobs, *j)
	}
	if paused != nil {
		paused.To, paused.Unended = lg.Last, true
		lg.Pauses = append(lg.Pauses, *paused)
	}
	sort.SliceStable(lg.Jobs, func(i, k int) bool { return lg.Jobs[i].Start.Before(lg.Jobs[k].Start) })
	setConcurrency(lg.Jobs)
	setWorkloads(lg.Jobs)
	return lg, nil
}

// refusedUnidentified reports whether a refusal line,
//
//	refused METHOD "PATH" from ADDR: REASON
//
// was the agent failing to identify the caller ("identify ADDR: ...")
// rather than a caller it identified and doesn't allow. Only REASON is
// checked, since the path is the caller's and may hold any text. Agents
// before 0.1.5 logged the path unquoted; there the first " from " ends it.
func refusedUnidentified(msg string) bool {
	_, rest, ok := strings.Cut(strings.TrimPrefix(msg, "refused "), " ") // method
	if !ok {
		return false
	}
	if q, err := strconv.QuotedPrefix(rest); err == nil {
		rest = strings.TrimPrefix(rest[len(q):], " from ")
	} else if _, rest, ok = strings.Cut(rest, " from "); !ok {
		return false
	}
	_, reason, ok := strings.Cut(rest, ": ") // the address
	return ok && strings.HasPrefix(reason, "identify ")
}

// maxLineBytes bounds a log line. A job's whole argv is logged, so a line
// can be huge; longer lines are skipped instead of failing the whole log.
const maxLineBytes = 1 << 20

type lineReader struct{ r *bufio.Reader }

func newLineReader(r io.Reader) *lineReader { return &lineReader{bufio.NewReaderSize(r, 64<<10)} }

// next returns the next line without its line ending, skipping lines
// longer than maxLineBytes.
func (lr *lineReader) next() (string, error) {
	for {
		var (
			buf     []byte
			tooLong bool
		)
		for {
			chunk, isPrefix, err := lr.r.ReadLine()
			if err != nil {
				if err == io.EOF && len(buf) > 0 && !tooLong {
					return string(buf), nil
				}
				return "", err
			}
			if !tooLong {
				if len(buf)+len(chunk) > maxLineBytes {
					tooLong, buf = true, nil
				} else {
					buf = append(buf, chunk...)
				}
			}
			if !isPrefix {
				break
			}
		}
		if !tooLong {
			return string(buf), nil
		}
	}
}

// splitLine separates the timestamp the standard logger writes
// ("2006/01/02 15:04:05", optionally with microseconds) from the message,
// and reports whether the timestamp has sub-second precision.
func splitLine(s string) (at time.Time, precise bool, msg string, ok bool) {
	date, rest, ok := strings.Cut(s, " ")
	if !ok {
		return time.Time{}, false, "", false
	}
	clock, msg, ok := strings.Cut(rest, " ")
	if !ok {
		return time.Time{}, false, "", false
	}
	// Parsing accepts a fractional second even though the layout has none.
	at, err := time.ParseInLocation("2006/01/02 15:04:05", date+" "+clock, time.Local)
	if err != nil {
		return time.Time{}, false, "", false
	}
	return at, strings.Contains(clock, "."), msg, true
}

// endTime estimates when a job really ended. With whole-second timestamps,
// each logged time is up to a second early: the end lies in [done, done+1s)
// and also in [start+d, start+d+1s), since the job ran for d. The middle of
// where both ranges overlap is the best estimate, typically within 0.25s
// instead of 0.5s.
func endTime(s started, done time.Time, precise bool, d time.Duration) time.Time {
	if precise || s.at.IsZero() {
		return done
	}
	lo, hi := done, done.Add(time.Second)
	if !s.precise {
		if t := s.at.Add(d); t.After(lo) {
			lo = t
		}
		if t := s.at.Add(d + time.Second); t.Before(hi) {
			hi = t
		}
	}
	if !lo.Before(hi) {
		return done.Add(time.Second / 2)
	}
	return lo.Add(hi.Sub(lo) / 2)
}

// parseArgv reads a []string printed with %q: ["a" "b c"].
func parseArgv(s string) ([]string, error) {
	s = strings.TrimSuffix(strings.TrimPrefix(s, "["), "]")
	var out []string
	for s = strings.TrimSpace(s); s != ""; s = strings.TrimSpace(s) {
		q, err := strconv.QuotedPrefix(s)
		if err != nil {
			return nil, err
		}
		arg, err := strconv.Unquote(q)
		if err != nil {
			return nil, err
		}
		out = append(out, arg)
		s = s[len(q):]
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty argv")
	}
	return out, nil
}

// setConcurrency sets each finished job's average number of jobs running at
// once over its lifetime.
func setConcurrency(jobs []Job) {
	type event struct {
		at    time.Time
		delta int
	}
	var evs []event
	for _, j := range jobs {
		if j.Finished() {
			evs = append(evs, event{j.Start, +1}, event{j.End, -1})
		}
	}
	// Ends before starts at the same instant: a job that ends exactly when
	// another starts did not overlap it.
	sort.Slice(evs, func(i, k int) bool {
		if evs[i].at.Equal(evs[k].at) {
			return evs[i].delta < evs[k].delta
		}
		return evs[i].at.Before(evs[k].at)
	})
	// running[i] is the count just after evs[i].
	running := make([]int, len(evs))
	n := 0
	for i, e := range evs {
		n += e.delta
		running[i] = n
	}
	for i := range jobs {
		j := &jobs[i]
		if !j.Finished() {
			continue
		}
		if j.Duration <= 0 {
			j.Concurrent = 1
			continue
		}
		// Count in effect at j.Start, then integrate across the events inside.
		k := sort.Search(len(evs), func(x int) bool { return evs[x].at.After(j.Start) })
		cur, t := 0, j.Start
		if k > 0 {
			cur = running[k-1]
		}
		var area float64
		for ; k < len(evs) && evs[k].at.Before(j.End); k++ {
			area += float64(cur) * evs[k].at.Sub(t).Seconds()
			cur, t = running[k], evs[k].at
		}
		area += float64(cur) * j.End.Sub(t).Seconds()
		j.Concurrent = max(1, int(area/j.Duration.Seconds()+0.5))
		// The agent never runs more than MaxJobs; more means timing blur.
		if j.MaxJobs > 0 {
			j.Concurrent = min(j.Concurrent, j.MaxJobs)
		}
	}
}

// setWorkloads names each job's workload. Jobs of the same program with the
// same number of arguments are compared position by position: a position
// whose values are many and none common (a task number from pool map) is the
// task argument and is shown as {}; the others stay, so different work sizes
// remain different workloads.
func setWorkloads(jobs []Job) {
	type shape struct {
		prog  string
		n     int
		shell bool
	}
	groups := map[shape][]int{}
	for i, j := range jobs {
		s := shape{strings.ToLower(j.Argv[0]), len(j.Argv), j.Shell}
		groups[s] = append(groups[s], i)
	}
	for _, idx := range groups {
		n := len(jobs[idx[0]].Argv)
		varies := make([]bool, n)
		for p := 1; p < n; p++ {
			counts := map[string]int{}
			top := 0
			for _, i := range idx {
				counts[jobs[i].Argv[p]]++
				top = max(top, counts[jobs[i].Argv[p]])
			}
			varies[p] = len(counts) > 10 && top*5 < len(idx)
		}
		for _, i := range idx {
			parts := make([]string, n)
			for p, a := range jobs[i].Argv {
				if p == 0 {
					a = jobs[idx[0]].Argv[0] // one spelling per workload
				}
				switch {
				case varies[p]:
					parts[p] = "{}"
				case strings.ContainsAny(a, " \t\""):
					parts[p] = strconv.Quote(a)
				default:
					parts[p] = a
				}
			}
			jobs[i].Workload = strings.Join(parts, " ")
		}
	}
}
