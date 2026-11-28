package agent

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/montjeffrey/compute-pool/internal/proto"
	"github.com/montjeffrey/compute-pool/internal/sysinfo"
)

func TestCriticalMemory(t *testing.T) {
	for _, tc := range []struct{ total, want int64 }{
		{4096, 512},   // small devices keep at least 512 MB
		{16384, 819},  // half of the 1.6 GB reserve
		{65536, 3276}, // scales with memory
	} {
		if got := CriticalMemoryMB(tc.total); got != tc.want {
			t.Errorf("CriticalMemoryMB(%d) = %d, want %d", tc.total, got, tc.want)
		}
	}
	if got := DefaultJobMemoryLimitMB(16384); got != 5461 {
		t.Errorf("DefaultJobMemoryLimitMB(16384) = %d, want 5461", got)
	}
	if got := DefaultJobMemoryLimitMB(2048); got != 1024 {
		t.Errorf("DefaultJobMemoryLimitMB(2048) = %d, want 1024", got)
	}
}

func TestRelieveMemoryStopsNewestFirst(t *testing.T) {
	var mu sync.Mutex
	snap := sysinfo.Snapshot{Memory: sysinfo.Memory{Known: true, TotalMB: 16384, FreeMB: 4000}}
	a, err := New(Config{
		NodeName: "desk", MaxJobs: 4, WorkDir: t.TempDir(), Auth: denyAuth{},
		Logger: log.New(io.Discard, "", 0),
		Probe: func() sysinfo.Snapshot {
			mu.Lock()
			defer mu.Unlock()
			return snap
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	setFree := func(mb int64) {
		mu.Lock()
		snap.Memory.FreeMB = mb
		mu.Unlock()
	}

	causes := map[string]error{}
	start := time.Now()
	for id, age := range map[string]time.Duration{"old": 0, "middle": time.Second, "newest": 2 * time.Second} {
		a.jobStarted(id, "laptop", proto.RunRequest{Argv: []string{"x"}})
		a.active[id].Started = start.Add(age)
		a.jobStoppable(id, func(cause error) { causes[id] = cause })
	}

	if a.relieveMemory() || len(causes) != 0 {
		t.Fatalf("stopped a job with 4 GB free: %v", causes)
	}
	setFree(500) // below the critical 819 MB
	for _, want := range []string{"newest", "middle", "old"} {
		if !a.relieveMemory() {
			t.Fatalf("no job stopped; want %q", want)
		}
		var low *lowMemoryError
		if !errors.As(causes[want], &low) || !strings.Contains(low.Error(), "desk ran low on memory") {
			t.Fatalf("job %q: cause = %v, want a lowMemoryError", want, causes[want])
		}
	}
	if a.relieveMemory() {
		t.Error("stopped a job twice")
	}
}

func TestStopCause(t *testing.T) {
	ctx, stop := context.WithCancelCause(context.Background())
	low := &lowMemoryError{node: "n", freeMB: 100, criticMB: 512}
	stop(low)
	if got := stopCause(ctx, nil, true); got != nil {
		t.Errorf("job that succeeded: cause %v, want nil", got)
	}
	if got := stopCause(ctx, errors.New("exit status 2"), false); got != nil {
		t.Errorf("job that failed before the guard's stop: cause %v, want nil", got)
	}
	if got := stopCause(ctx, errors.New("killed"), true); got != low {
		t.Errorf("job the guard killed: cause %v, want the guard's", got)
	}
	ctx, stop = context.WithCancelCause(context.Background())
	over := &memoryLimitError{node: "n", limitMB: 128}
	stop(over)
	if got := stopCause(ctx, errors.New("exit status 2"), false); got != over {
		t.Errorf("job that failed past its memory limit: cause %v, want the limit", got)
	}
	if code, err := memoryStop(over); code != exitLowMemory || err != over {
		t.Errorf("memoryStop = %d, %v, want %d and the limit", code, err, exitLowMemory)
	}
	if code, err := memoryStop(nil); code != 0 || err != nil {
		t.Errorf("memoryStop(nil) = %d, %v, want 0, nil", code, err)
	}
}

// TestKilledByStop checks that only a process Cancel ended counts as
// killed: one that exits by itself with 1, the code taskkill /F leaves on
// Windows, or 2, doesn't.
func TestKilledByStop(t *testing.T) {
	helper := func(mode string) *exec.Cmd {
		cmd := buildCommand(context.Background(), proto.RunRequest{Argv: []string{os.Args[0], "-test.run=^TestHelperProcess$", "--", mode}})
		cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")
		return cmd
	}
	for _, mode := range []string{"fail", "fail1"} {
		own := helper(mode)
		own.Dir = t.TempDir()
		if err := own.Run(); err == nil {
			t.Fatalf("%s helper exited 0", mode)
		}
		if killedByStop(own.ProcessState) {
			t.Errorf("a job that exited %d itself counts as killed", own.ProcessState.ExitCode())
		}
	}
	stopped := helper("sleep")
	if err := stopped.Start(); err != nil {
		t.Fatal(err)
	}
	if err := stopped.Cancel(); err != nil {
		t.Fatal(err)
	}
	stopped.Wait()
	if !killedByStop(stopped.ProcessState) {
		t.Errorf("a job Cancel ended doesn't count as killed (%v)", stopped.ProcessState)
	}
	if !killedByStop(nil) {
		t.Error("a job never waited for doesn't count as killed")
	}
}
