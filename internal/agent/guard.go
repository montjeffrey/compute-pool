package agent

import (
	"context"
	"fmt"
	"time"
)

// The live limit keeps the agent from starting jobs when memory is short,
// but jobs already running can keep growing, and the owner can open more
// programs. When free memory falls below the critical level the device
// starts paging hard and can stop responding, so the guard stops the
// agent's newest job, waits for its memory to come back, and repeats while
// memory stays critical. The newest job has done the least work, so
// stopping it loses the least.

// CriticalMemoryMB is the free memory below which the guard stops jobs: half
// the reserve the live limit keeps (at least 512 MB).
func CriticalMemoryMB(totalMB int64) int64 { return memoryReserveMB(totalMB) / 2 }

// guardSettle is how many guard intervals to wait after stopping a job, so
// the next check sees the memory it gave back.
const guardSettle = 5

// lowMemoryError is why the guard stopped a job.
type lowMemoryError struct {
	node             string
	freeMB, criticMB int64
}

func (e *lowMemoryError) Error() string {
	return fmt.Sprintf("stopped: %s ran low on memory (%s free, below %s); the job was ended to keep the device responsive",
		e.node, gb(e.freeMB), gb(e.criticMB))
}

// memoryLimitError is why a job that reached Config.JobMemoryLimitMB was
// stopped.
type memoryLimitError struct {
	node    string
	limitMB int64
}

func (e *memoryLimitError) Error() string {
	return fmt.Sprintf("stopped: the job reached its memory limit of %d MB on %s (--job-memory-limit)", e.limitMB, e.node)
}

// GuardMemory checks free memory every interval until ctx ends, stopping
// the newest job whenever it is critically low. It does nothing without a
// Probe that reports memory.
func (a *Agent) GuardMemory(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	wait := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if wait > 0 {
			wait--
			continue
		}
		if a.relieveMemory() {
			wait = guardSettle
		}
	}
}

// relieveMemory stops the newest running job if free memory is critically
// low, and reports whether it stopped one.
func (a *Agent) relieveMemory() bool {
	m := a.snapshot().Memory
	if !m.Known || m.TotalMB <= 0 {
		return false
	}
	critical := CriticalMemoryMB(m.TotalMB)
	if m.FreeMB >= critical {
		return false
	}
	a.histMu.Lock()
	newest := ""
	for id, rec := range a.active {
		if a.stops[id] == nil {
			continue
		}
		if newest == "" || rec.Started.After(a.active[newest].Started) {
			newest = id
		}
	}
	stop := a.stops[newest]
	delete(a.stops, newest) // stop each job once
	a.histMu.Unlock()
	if stop == nil {
		return false
	}
	a.cfg.Logger.Printf("job %s stopped: %s of memory free, below the critical %s", newest, gb(m.FreeMB), gb(critical))
	stop(&lowMemoryError{node: a.cfg.NodeName, freeMB: m.FreeMB, criticMB: critical})
	return true
}
