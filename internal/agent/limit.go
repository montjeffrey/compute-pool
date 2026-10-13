package agent

import (
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/montjeffrey/compute-pool/internal/sysinfo"
)

// How many jobs the agent takes is decided again for every job: MaxJobs is
// the most it ever runs, and the live limit is lower while the owner's own
// programs use the CPU or free memory runs short. Jobs already running are
// never stopped; the agent just takes no more until there is room.

const (
	// DefaultJobMemoryMB is the memory assumed per job until the agent has
	// measured some.
	DefaultJobMemoryMB = 512
	// minJobMemoryMB keeps tiny measured jobs from making the memory limit
	// meaningless.
	minJobMemoryMB = 16
	// memSamples is how many recent jobs the memory estimate looks at.
	memSamples = 20
	// rampTime is how long a new job may take to allocate its memory. Until
	// then free memory doesn't show it, so the estimate is set aside for it.
	rampTime = 10 * time.Second
)

// memEstimate learns how much memory jobs need from recent jobs' peaks.
type memEstimate struct {
	mu    sync.Mutex
	def   int64   // MB assumed before any job has been measured
	peaks []int64 // MB, most recent last
}

// observe records a finished job's peak memory use in bytes.
func (m *memEstimate) observe(bytes uint64) {
	if bytes == 0 {
		return
	}
	mb := int64((bytes + 1<<20 - 1) >> 20)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.peaks = append(m.peaks, mb)
	if len(m.peaks) > memSamples {
		m.peaks = m.peaks[len(m.peaks)-memSamples:]
	}
}

// get returns the memory in MB to plan for each new job, and whether it was
// measured rather than assumed. It is the largest recent peak, so a few
// memory-hungry jobs aren't packed in on the strength of many small ones.
func (m *memEstimate) get() (int64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.peaks) == 0 {
		return m.def, false
	}
	most := int64(minJobMemoryMB)
	for _, p := range m.peaks {
		most = max(most, p)
	}
	return most, true
}

// memoryReserveMB is the free memory kept for the owner: the larger of 1 GB
// and a tenth of the device's memory. Programs already open are excluded
// from free memory, so this is room for whatever the owner starts next.
func memoryReserveMB(totalMB int64) int64 { return max(1024, totalMB/10) }

type limitInput struct {
	maxJobs  int
	cpus     int
	snap     sysinfo.Snapshot
	admitted int   // jobs holding a slot, running or starting
	ramping  int   // those admitted within rampTime
	jobMemMB int64 // memory to plan for each job
}

// jobLimit returns how many jobs, counting those already admitted, the agent
// would run at once right now, and why that is below maxJobs ("" if not).
func jobLimit(in limitInput) (int, string) {
	limit, why := in.maxJobs, ""
	// CPU: the threads other programs leave idle. Only when the agent can
	// tell its own jobs' use apart; otherwise every job it started would
	// lower the limit further.
	if s := in.snap; s.CPUBusyPct >= 0 && s.OwnBusyPct >= 0 && in.cpus > 0 {
		used := s.OtherBusyPct() * float64(in.cpus) / 100
		if l := in.cpus - int(math.Round(used)); l < limit {
			limit, why = l, fmt.Sprintf("other programs are using %.1f of %d threads", used, in.cpus)
		}
	}
	// Memory: what's free after the owner's reserve and what starting jobs
	// are still expected to allocate, divided among new jobs.
	if m := in.snap.Memory; m.Known && in.jobMemMB > 0 {
		reserve := memoryReserveMB(m.TotalMB)
		left := m.FreeMB - reserve - int64(in.ramping)*in.jobMemMB
		room := int(math.Floor(float64(left) / float64(in.jobMemMB)))
		if l := in.admitted + room; l < limit {
			limit, why = l, fmt.Sprintf("%s of memory free, %s kept for you, about %d MB per job",
				gb(m.FreeMB), gb(reserve), in.jobMemMB)
		}
	}
	return max(0, limit), why
}

// fullReason explains why a job was refused at limit.
func fullReason(limit int, why string) string {
	switch {
	case why == "":
		return fmt.Sprintf("all %d job slots in use", limit)
	case limit == 0:
		return "taking no jobs for now: " + why
	default:
		return fmt.Sprintf("at its limit of %d jobs for now: %s", limit, why)
	}
}

func gb(mb int64) string { return fmt.Sprintf("%.1f GB", float64(mb)/1024) }

// slotState is the live limit and how many slots are taken.
type slotState struct {
	limit    int
	why      string
	admitted int
	jobMemMB int64
	measured bool // jobMemMB comes from finished jobs
}

// slotsLocked works out the live limit. a.admitMu must be held.
func (a *Agent) slotsLocked(s sysinfo.Snapshot, now time.Time) slotState {
	ramping := 0
	for _, t := range a.admitted {
		if now.Sub(t) < rampTime {
			ramping++
		}
	}
	mem, measured := a.mem.get()
	limit, why := jobLimit(limitInput{
		maxJobs:  a.cfg.MaxJobs,
		cpus:     a.cpus,
		snap:     s,
		admitted: len(a.admitted),
		ramping:  ramping,
		jobMemMB: mem,
	})
	if limit != a.lastLimit {
		if why == "" {
			a.cfg.Logger.Printf("job limit now %d", limit)
		} else {
			a.cfg.Logger.Printf("job limit now %d: %s", limit, why)
		}
		a.lastLimit = limit
	}
	return slotState{limit: limit, why: why, admitted: len(a.admitted), jobMemMB: mem, measured: measured}
}

func (a *Agent) slots(s sysinfo.Snapshot) slotState {
	a.admitMu.Lock()
	defer a.admitMu.Unlock()
	return a.slotsLocked(s, time.Now())
}

// admit takes a job slot if the agent is below its live limit. It returns a
// function that gives the slot back, or nil and the reason for refusing.
func (a *Agent) admit(s sysinfo.Snapshot) (func(), string) {
	a.admitMu.Lock()
	defer a.admitMu.Unlock()
	now := time.Now()
	st := a.slotsLocked(s, now)
	if st.admitted >= st.limit {
		return nil, fullReason(st.limit, st.why)
	}
	a.ticket++
	t := a.ticket
	a.admitted[t] = now
	return func() {
		a.admitMu.Lock()
		delete(a.admitted, t)
		a.admitMu.Unlock()
	}, ""
}
