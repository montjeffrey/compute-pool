// Package fanout spreads independent tasks over the job slots of several
// devices. Each slot pulls the next task as soon as it is free, so faster or
// less busy devices naturally take more of the work.
package fanout

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Task is one unit of work.
type Task struct {
	Index int    // 1-based position in the input
	Item  string // the value substituted into the command
}

// Outcome is what a device reports for a task it ran.
type Outcome struct {
	ExitCode    int
	Stdout      []byte
	Stderr      []byte
	RemoteError string
	Files       []string
	FilesDir    string // where this attempt's collected files were written
	Duration    time.Duration
}

// Device runs tasks one per slot.
type Device struct {
	Name  string
	Slots int
	// Run executes a task. An error for which Options.Declined is true means
	// the device refused it before starting (paused, full) and the task will
	// go to another slot. Any other error means the device failed; the task
	// is retried elsewhere and this slot stops taking work.
	Run func(ctx context.Context, t Task) (Outcome, error)
}

// Result is the final state of a task.
type Result struct {
	Task
	Outcome
	Device     string
	Attempts   int   // runs that reached a device (refusals not counted)
	Speculated bool  // a backup copy was started on another device
	Err        error // non-nil if the task never completed
}

// OK reports whether the task ran and exited 0 with no agent-side error.
func (r Result) OK() bool {
	return r.Err == nil && r.ExitCode == 0 && r.RemoteError == ""
}

// Options tunes Run.
type Options struct {
	// Retries is how many more times a task may be attempted after a device
	// fails while running it.
	Retries int
	// Declined classifies device refusals. Nil means no error is a refusal.
	Declined func(error) bool
	// Backoff is how long a slot waits after its device declines. Default 2s.
	Backoff time.Duration
	// MaxParallel caps tasks in flight across all devices. 0 means no cap.
	MaxParallel int
	// Speculate starts a backup copy of a straggling task on another device
	// once nothing is left to start. The first copy to finish wins and the
	// other is canceled. Use only for tasks whose sole effect is their output.
	Speculate bool
	// OnResult is called once per task as it finishes, never concurrently.
	OnResult func(Result)
	// OnDiscard receives the outcome of a copy that finished after its task
	// was already done, so its files can be cleaned up.
	OnDiscard func(Outcome)
}

type attempt struct {
	device string
	start  time.Time
	cancel context.CancelFunc
}

type queued struct {
	Task
	pos        int
	attempts   int
	active     map[int]*attempt
	speculated bool
	finished   bool
}

type queue struct {
	mu        sync.Mutex
	cond      *sync.Cond
	pending   []*queued
	running   map[*queued]bool
	remaining int
	nextID    int
	speculate bool
	durations []time.Duration            // wall time of finished attempts
	byDevice  map[string][]time.Duration // the same, per device
}

func median(ds []time.Duration) time.Duration {
	sorted := append([]time.Duration(nil), ds...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[len(sorted)/2]
}

// acquire hands device a pending task or, when speculating and nothing is
// pending, a straggler running on another device. It blocks until there is
// work, everything is done, or ctx ends.
func (q *queue) acquire(ctx context.Context, device string) (*queued, int, context.Context, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for {
		if q.remaining == 0 || ctx.Err() != nil {
			return nil, 0, nil, false
		}
		var t *queued
		if len(q.pending) > 0 {
			t, q.pending = q.pending[0], q.pending[1:]
		} else if q.speculate {
			if t = q.straggler(device); t != nil {
				t.speculated = true
			}
		}
		if t != nil {
			actx, cancel := context.WithCancel(ctx)
			q.nextID++
			if t.active == nil {
				t.active = make(map[int]*attempt)
			}
			t.active[q.nextID] = &attempt{device: device, start: time.Now(), cancel: cancel}
			q.running[t] = true
			return t, q.nextID, actx, true
		}
		q.cond.Wait()
	}
}

// straggler picks the longest-running task that has run longer than the
// median finished attempt, has no backup yet, and is not on device. Only a
// device that has itself finished tasks at least that fast may run backups:
// a copy on a slower device would only waste work.
func (q *queue) straggler(device string) *queued {
	if len(q.durations) < 3 || len(q.byDevice[device]) == 0 {
		return nil
	}
	threshold := median(q.durations)
	if median(q.byDevice[device]) > threshold {
		return nil
	}

	var best *queued
	var bestAge time.Duration
	for t := range q.running {
		if t.finished || t.speculated || len(t.active) != 1 {
			continue
		}
		for _, a := range t.active {
			if age := time.Since(a.start); a.device != device && age > threshold && age > bestAge {
				best, bestAge = t, age
			}
		}
	}
	return best
}

// release ends one attempt. It reports when the attempt started, whether the
// task was already finished by another copy, and how many copies remain.
func (q *queue) release(t *queued, id int, counted bool) (start time.Time, finished bool, others int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if a, ok := t.active[id]; ok {
		start = a.start
		a.cancel()
		delete(t.active, id)
	}
	if counted && !t.finished {
		t.attempts++
	}
	if len(t.active) == 0 {
		delete(q.running, t)
	}
	return start, t.finished, len(t.active)
}

// requeue returns t to the front of the queue unless another copy is still
// running it.
func (q *queue) requeue(t *queued) {
	q.mu.Lock()
	if !t.finished && len(t.active) == 0 {
		q.pending = append([]*queued{t}, q.pending...)
	}
	q.mu.Unlock()
	q.cond.Broadcast()
}

// finish marks t done and cancels any other copy. It returns false if
// another copy got there first. wall and device describe the winning run.
func (q *queue) finish(t *queued, wall time.Duration, device string) bool {
	q.mu.Lock()
	if t.finished {
		q.mu.Unlock()
		return false
	}
	t.finished = true
	for _, a := range t.active {
		a.cancel()
	}
	if wall > 0 {
		q.durations = append(q.durations, wall)
		q.byDevice[device] = append(q.byDevice[device], wall)
	}
	q.remaining--
	q.mu.Unlock()
	q.cond.Broadcast()
	return true
}

// Run executes every task and returns results in input order. It returns
// when all tasks finished, ctx was canceled, or no device is left.
func Run(ctx context.Context, tasks []Task, devices []Device, opts Options) []Result {
	if opts.Backoff <= 0 {
		opts.Backoff = 2 * time.Second
	}
	declined := opts.Declined
	if declined == nil {
		declined = func(error) bool { return false }
	}

	results := make([]Result, len(tasks))
	all := make([]*queued, len(tasks))
	q := &queue{remaining: len(tasks), running: map[*queued]bool{}, speculate: opts.Speculate, byDevice: map[string][]time.Duration{}}
	q.cond = sync.NewCond(&q.mu)
	for i, t := range tasks {
		all[i] = &queued{Task: t, pos: i}
		q.pending = append(q.pending, all[i])
	}
	stop := context.AfterFunc(ctx, q.cond.Broadcast)
	defer stop()
	if opts.Speculate {
		// Stragglers become eligible as time passes, not on any event, so
		// waiting slots re-check periodically.
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		done := make(chan struct{})
		defer close(done)
		go func() {
			for {
				select {
				case <-tick.C:
					q.cond.Broadcast()
				case <-done:
					return
				}
			}
		}()
	}

	var sem chan struct{}
	if opts.MaxParallel > 0 {
		sem = make(chan struct{}, opts.MaxParallel)
	}

	var (
		resMu     sync.Mutex
		deviceErr = map[string]error{}
	)
	report := func(t *queued, r Result) {
		q.mu.Lock()
		r.Task, r.Attempts, r.Speculated = t.Task, t.attempts, t.speculated
		q.mu.Unlock()
		resMu.Lock()
		results[t.pos] = r
		if opts.OnResult != nil {
			opts.OnResult(r)
		}
		resMu.Unlock()
	}
	discard := func(out Outcome) {
		if opts.OnDiscard != nil {
			opts.OnDiscard(out)
		}
	}

	var wg sync.WaitGroup
	for _, d := range devices {
		for s := 0; s < d.Slots; s++ {
			wg.Add(1)
			go func(d Device) {
				defer wg.Done()
				for {
					t, id, actx, ok := q.acquire(ctx, d.Name)
					if !ok {
						return
					}
					if sem != nil {
						select {
						case sem <- struct{}{}:
						case <-actx.Done():
							q.release(t, id, false)
							q.requeue(t)
							if ctx.Err() != nil {
								return
							}
							continue
						}
					}
					out, err := d.Run(actx, t.Task)
					if sem != nil {
						<-sem
					}
					isDecline := err != nil && ctx.Err() == nil && declined(err)
					counted := ctx.Err() == nil && !isDecline
					start, finished, others := q.release(t, id, counted)
					if finished {
						// Another copy won; this one was canceled or lost the race.
						if err == nil {
							discard(out)
						}
						continue
					}
					switch {
					case err == nil:
						if q.finish(t, time.Since(start), d.Name) {
							report(t, Result{Outcome: out, Device: d.Name})
						} else {
							discard(out)
						}
					case ctx.Err() != nil:
						q.requeue(t) // reported as canceled below
						return
					case isDecline:
						q.requeue(t)
						select {
						case <-time.After(opts.Backoff):
						case <-ctx.Done():
							return
						}
					default:
						resMu.Lock()
						deviceErr[d.Name] = err
						resMu.Unlock()
						if others == 0 {
							q.mu.Lock()
							exhausted := t.attempts > opts.Retries
							q.mu.Unlock()
							if exhausted {
								if q.finish(t, 0, "") {
									report(t, Result{Device: d.Name, Err: fmt.Errorf("%s: %w", d.Name, err)})
								}
							} else {
								q.requeue(t)
							}
						}
						return // stop sending work to a device that just failed
					}
				}
			}(d)
		}
	}
	wg.Wait()

	// Anything unfinished was canceled or ran out of devices.
	reason := ctx.Err()
	if reason == nil {
		reason = noDevicesLeft(deviceErr)
	}
	for _, t := range all {
		if !t.finished {
			t.finished = true
			report(t, Result{Err: reason})
		}
	}
	return results
}

func noDevicesLeft(deviceErr map[string]error) error {
	if len(deviceErr) == 0 {
		return errors.New("no device available to run it")
	}
	names := make([]string, 0, len(deviceErr))
	for n := range deviceErr {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = fmt.Sprintf("%s: %v", n, deviceErr[n])
	}
	return fmt.Errorf("no device left to run it (%s)", strings.Join(parts, "; "))
}
