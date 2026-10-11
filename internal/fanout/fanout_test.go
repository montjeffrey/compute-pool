package fanout

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var errDeclined = errors.New("declined")

func tasks(n int) []Task {
	ts := make([]Task, n)
	for i := range ts {
		ts[i] = Task{Index: i + 1, Item: fmt.Sprint(i + 1)}
	}
	return ts
}

// okDevice echoes the item after a short delay and counts how many tasks it ran.
func okDevice(name string, slots int, delay time.Duration, ran *atomic.Int32) Device {
	return Device{Name: name, Slots: slots, Run: func(ctx context.Context, t Task) (Outcome, error) {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return Outcome{}, ctx.Err()
		}
		ran.Add(1)
		return Outcome{Stdout: []byte(t.Item)}, nil
	}}
}

func opts() Options {
	return Options{Retries: 2, Backoff: 10 * time.Millisecond, Declined: func(err error) bool { return errors.Is(err, errDeclined) }}
}

func TestSpreadsAcrossDevicesInOrder(t *testing.T) {
	var a, b atomic.Int32
	res := Run(context.Background(), tasks(40), []Device{
		okDevice("a", 4, 5*time.Millisecond, &a),
		okDevice("b", 2, 5*time.Millisecond, &b),
	}, opts())
	if len(res) != 40 {
		t.Fatalf("got %d results", len(res))
	}
	for i, r := range res {
		if !r.OK() || r.Index != i+1 || string(r.Stdout) != fmt.Sprint(i+1) || r.Attempts != 1 {
			t.Fatalf("result %d = %+v", i, r)
		}
	}
	if a.Load()+b.Load() != 40 || a.Load() == 0 || b.Load() == 0 {
		t.Errorf("ran a=%d b=%d; want both used, 40 total", a.Load(), b.Load())
	}
	if a.Load() <= b.Load() {
		t.Errorf("device with more slots should take more work: a=%d b=%d", a.Load(), b.Load())
	}
}

func TestDecliningDeviceHandsWorkToOthers(t *testing.T) {
	var ran atomic.Int32
	paused := Device{Name: "paused", Slots: 3, Run: func(context.Context, Task) (Outcome, error) {
		return Outcome{}, errDeclined
	}}
	res := Run(context.Background(), tasks(10), []Device{paused, okDevice("ok", 1, time.Millisecond, &ran)}, opts())
	for _, r := range res {
		if !r.OK() || r.Device != "ok" || r.Attempts != 1 {
			t.Errorf("result = %+v", r)
		}
	}
}

func TestFailingDeviceTasksRetryElsewhere(t *testing.T) {
	var ran atomic.Int32
	var brokenCalls atomic.Int32
	broken := Device{Name: "broken", Slots: 2, Run: func(context.Context, Task) (Outcome, error) {
		brokenCalls.Add(1)
		return Outcome{}, errors.New("connection lost")
	}}
	res := Run(context.Background(), tasks(8), []Device{broken, okDevice("ok", 1, time.Millisecond, &ran)}, opts())
	for _, r := range res {
		if !r.OK() || r.Device != "ok" {
			t.Errorf("result = %+v", r)
		}
	}
	// Each of the broken device's slots fails once, then stops taking work.
	if n := brokenCalls.Load(); n > 2 {
		t.Errorf("broken device called %d times, want at most 2", n)
	}
}

func TestRetriesExhaustedAndNoDevicesLeft(t *testing.T) {
	broken := Device{Name: "broken", Slots: 1, Run: func(context.Context, Task) (Outcome, error) {
		return Outcome{}, errors.New("connection refused")
	}}
	o := opts()
	o.Retries = 0
	done := make(chan []Result)
	go func() { done <- Run(context.Background(), tasks(3), []Device{broken}, o) }()
	select {
	case res := <-done:
		for _, r := range res {
			if r.Err == nil || r.OK() {
				t.Errorf("result = %+v, want an error", r)
			}
		}
		if !strings.Contains(res[0].Err.Error(), "connection refused") {
			t.Errorf("first task error = %v", res[0].Err)
		}
		if !strings.Contains(res[2].Err.Error(), "no device left") {
			t.Errorf("last task error = %v", res[2].Err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run hung with no devices left")
	}
}

func TestNonZeroExitIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	d := Device{Name: "d", Slots: 1, Run: func(context.Context, Task) (Outcome, error) {
		calls.Add(1)
		return Outcome{ExitCode: 3}, nil
	}}
	res := Run(context.Background(), tasks(1), []Device{d}, opts())
	if res[0].OK() || res[0].ExitCode != 3 || res[0].Err != nil || calls.Load() != 1 {
		t.Errorf("result = %+v, calls = %d", res[0], calls.Load())
	}
}

func TestMaxParallel(t *testing.T) {
	var cur, peak atomic.Int32
	d := Device{Name: "d", Slots: 8, Run: func(context.Context, Task) (Outcome, error) {
		n := cur.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		cur.Add(-1)
		return Outcome{}, nil
	}}
	o := opts()
	o.MaxParallel = 3
	Run(context.Background(), tasks(30), []Device{d}, o)
	if peak.Load() > 3 {
		t.Errorf("peak concurrency %d, want <= 3", peak.Load())
	}
}

func TestSpeculationRescuesStraggler(t *testing.T) {
	var fastRan atomic.Int32
	var slowCanceled atomic.Bool
	slow := Device{Name: "slow", Slots: 1, Run: func(ctx context.Context, t Task) (Outcome, error) {
		select {
		case <-time.After(10 * time.Second):
			return Outcome{Stdout: []byte("slow")}, nil
		case <-ctx.Done():
			slowCanceled.Store(true)
			return Outcome{}, ctx.Err()
		}
	}}
	o := opts()
	o.Speculate = true
	var discarded atomic.Int32
	o.OnDiscard = func(Outcome) { discarded.Add(1) }
	start := time.Now()
	res := Run(context.Background(), tasks(12), []Device{slow, okDevice("fast", 2, 20*time.Millisecond, &fastRan)}, o)
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("took %s; the straggler was not rescued", took)
	}
	backups := 0
	for _, r := range res {
		if !r.OK() || r.Device != "fast" {
			t.Errorf("result = %+v", r)
		}
		if r.Speculated {
			backups++
		}
	}
	if backups != 1 {
		t.Errorf("%d tasks speculated, want 1", backups)
	}
	waitUntil(t, slowCanceled.Load)
	if discarded.Load() != 0 {
		t.Errorf("OnDiscard called %d times for a canceled copy", discarded.Load())
	}
}

func TestNoSpeculationByDefault(t *testing.T) {
	var fastRan atomic.Int32
	slow := Device{Name: "slow", Slots: 1, Run: func(ctx context.Context, t Task) (Outcome, error) {
		time.Sleep(300 * time.Millisecond)
		return Outcome{}, nil
	}}
	res := Run(context.Background(), tasks(8), []Device{slow, okDevice("fast", 2, 10*time.Millisecond, &fastRan)}, opts())
	for _, r := range res {
		if r.Speculated || r.Attempts != 1 {
			t.Errorf("result = %+v", r)
		}
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCancelStopsPromptly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var ran atomic.Int32
	var mu sync.Mutex
	var reported []Result
	o := opts()
	o.OnResult = func(r Result) { mu.Lock(); reported = append(reported, r); mu.Unlock() }
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()
	start := time.Now()
	res := Run(ctx, tasks(100), []Device{okDevice("slow", 2, 20*time.Millisecond, &ran)}, o)
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("Run took %s after cancel", took)
	}
	canceled := 0
	for _, r := range res {
		if errors.Is(r.Err, context.Canceled) {
			canceled++
		}
	}
	if canceled == 0 {
		t.Error("no task reported as canceled")
	}
	if len(reported) != 100 {
		t.Errorf("OnResult called %d times, want 100", len(reported))
	}
}
