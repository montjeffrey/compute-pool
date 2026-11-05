package agent

import (
	"errors"
	"io"
	"log"
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
