package agent

import (
	"context"
	"io"
	"log"
	"strings"
	"testing"

	"github.com/montjeffrey/compute-pool/internal/sysinfo"
)

func mem(totalMB, freeMB int64) sysinfo.Memory {
	return sysinfo.Memory{Known: true, TotalMB: totalMB, FreeMB: freeMB}
}

func TestJobLimit(t *testing.T) {
	unknown := sysinfo.Snapshot{CPUBusyPct: -1, OwnBusyPct: -1}
	tests := []struct {
		name     string
		in       limitInput
		want     int
		whyHas   string
		whyEmpty bool
	}{
		{name: "nothing known", in: limitInput{maxJobs: 15, cpus: 16, snap: unknown, jobMemMB: 512}, want: 15, whyEmpty: true},
		{name: "idle", in: limitInput{maxJobs: 15, cpus: 16, snap: sysinfo.Snapshot{CPUBusyPct: 70, OwnBusyPct: 68, Memory: mem(32768, 20000)}, jobMemMB: 512},
			want: 15, whyEmpty: true},
		{name: "owner uses a quarter of the threads",
			in:   limitInput{maxJobs: 15, cpus: 16, snap: sysinfo.Snapshot{CPUBusyPct: 25, OwnBusyPct: 0}, jobMemMB: 512},
			want: 12, whyHas: "other programs are using 4.0 of 16 threads"},
		{name: "own use unknown: CPU doesn't limit",
			in:   limitInput{maxJobs: 15, cpus: 16, snap: sysinfo.Snapshot{CPUBusyPct: 90, OwnBusyPct: -1}, jobMemMB: 512},
			want: 15, whyEmpty: true},
		{name: "memory: room for 8 more beside 2 settled jobs",
			// 6000 free - 1638 reserve = 4362 MB, 8 jobs of 512 MB.
			in:   limitInput{maxJobs: 15, cpus: 16, snap: sysinfo.Snapshot{CPUBusyPct: -1, OwnBusyPct: -1, Memory: mem(16384, 6000)}, admitted: 2, jobMemMB: 512},
			want: 10, whyHas: "5.9 GB of memory free, 1.6 GB kept for you, about 512 MB per job"},
		{name: "memory: starting jobs haven't allocated yet",
			in:   limitInput{maxJobs: 15, cpus: 16, snap: sysinfo.Snapshot{CPUBusyPct: -1, OwnBusyPct: -1, Memory: mem(16384, 6000)}, admitted: 2, ramping: 2, jobMemMB: 512},
			want: 8, whyHas: "memory"},
		{name: "memory below the reserve stops new jobs",
			in:   limitInput{maxJobs: 15, cpus: 16, snap: sysinfo.Snapshot{CPUBusyPct: -1, OwnBusyPct: -1, Memory: mem(16384, 1000)}, admitted: 3, jobMemMB: 512},
			want: 1, whyHas: "memory"},
		{name: "never below zero",
			in:   limitInput{maxJobs: 15, cpus: 16, snap: sysinfo.Snapshot{CPUBusyPct: -1, OwnBusyPct: -1, Memory: mem(8192, 100)}, jobMemMB: 512},
			want: 0, whyHas: "memory"},
		{name: "unmeasured guess too big: one job to learn from",
			in:   limitInput{maxJobs: 15, cpus: 16, snap: sysinfo.Snapshot{CPUBusyPct: -1, OwnBusyPct: -1, Memory: mem(16384, 2000)}, jobMemMB: 512},
			want: 1, whyHas: "one job at a time until it learns"},
		{name: "unmeasured: only one job at a time",
			in:   limitInput{maxJobs: 15, cpus: 16, snap: sysinfo.Snapshot{CPUBusyPct: -1, OwnBusyPct: -1, Memory: mem(16384, 2000)}, admitted: 1, ramping: 1, jobMemMB: 512},
			want: 0, whyHas: "memory"},
		{name: "measured big jobs don't get a probe",
			in:   limitInput{maxJobs: 15, cpus: 16, snap: sysinfo.Snapshot{CPUBusyPct: -1, OwnBusyPct: -1, Memory: mem(16384, 2000)}, jobMemMB: 512, measured: true},
			want: 0, whyHas: "about 512 MB per job"},
		{name: "small devices keep 1 GB",
			in:   limitInput{maxJobs: 4, cpus: 4, snap: sysinfo.Snapshot{CPUBusyPct: -1, OwnBusyPct: -1, Memory: mem(4096, 2048)}, jobMemMB: 512},
			want: 2, whyHas: "1.0 GB kept for you"},
		{name: "tighter of CPU and memory wins",
			in:   limitInput{maxJobs: 15, cpus: 16, snap: sysinfo.Snapshot{CPUBusyPct: 50, OwnBusyPct: 0, Memory: mem(16384, 4000)}, jobMemMB: 512},
			want: 4, whyHas: "memory"},
		{name: "measured small jobs: CPU decides",
			in:   limitInput{maxJobs: 15, cpus: 16, snap: sysinfo.Snapshot{CPUBusyPct: 50, OwnBusyPct: 0, Memory: mem(16384, 4000)}, jobMemMB: 16},
			want: 8, whyHas: "8.0 of 16 threads"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, why := jobLimit(tt.in)
			if got != tt.want {
				t.Errorf("limit = %d (%s), want %d", got, why, tt.want)
			}
			if tt.whyEmpty && why != "" {
				t.Errorf("why = %q, want none", why)
			}
			if tt.whyHas != "" && !strings.Contains(why, tt.whyHas) {
				t.Errorf("why = %q, want it to mention %q", why, tt.whyHas)
			}
		})
	}
}

func TestMemEstimate(t *testing.T) {
	m := memEstimate{def: 512}
	if got, measured := m.get(); got != 512 || measured {
		t.Fatalf("before any job: %d, %v", got, measured)
	}
	m.observe(0) // unknown: ignored
	if _, measured := m.get(); measured {
		t.Fatal("a job with unknown memory counted as measured")
	}
	m.observe(100 << 20)
	m.observe(300<<20 + 1) // rounds up
	m.observe(50 << 20)
	if got, measured := m.get(); got != 301 || !measured {
		t.Errorf("after jobs: %d, %v; want the largest, 301", got, measured)
	}
	for range memSamples {
		m.observe(1 << 10)
	}
	if got, _ := m.get(); got != minJobMemoryMB {
		t.Errorf("after the big jobs aged out: %d, want the %d MB floor", got, minJobMemoryMB)
	}
}

func TestFullReason(t *testing.T) {
	if got := fullReason(4, ""); got != "all 4 job slots in use" {
		t.Errorf("at MaxJobs: %q", got)
	}
	if got := fullReason(0, "x"); got != "taking no jobs for now: x" {
		t.Errorf("at zero: %q", got)
	}
	if got := fullReason(3, "x"); got != "at its limit of 3 jobs for now: x" {
		t.Errorf("lowered: %q", got)
	}
}

type denyAuth struct{}

func (denyAuth) Authorize(context.Context, string) (Identity, error) { return Identity{}, ErrForbidden }

func TestAdmitFollowsLiveLimit(t *testing.T) {
	a, err := New(Config{MaxJobs: 8, WorkDir: t.TempDir(), Auth: denyAuth{}, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	a.cpus = 16
	// Room for exactly two jobs of the default 512 MB beyond the reserve.
	snap := sysinfo.Snapshot{CPUBusyPct: -1, OwnBusyPct: -1, Memory: mem(16384, 1638+2*512)}
	r1, why := a.admit(snap)
	if r1 == nil {
		t.Fatalf("first job refused: %s", why)
	}
	r2, why := a.admit(snap)
	if r2 == nil {
		t.Fatalf("second job refused: %s", why)
	}
	// Free memory hasn't dropped yet, but the two starting jobs are
	// expected to use it.
	if r, why := a.admit(snap); r != nil || !strings.HasPrefix(why, "at its limit of 2 jobs for now") {
		t.Fatalf("third job: admitted=%v why=%q", r != nil, why)
	}
	r1()
	r3, why := a.admit(snap)
	if r3 == nil {
		t.Fatalf("job after a slot freed refused: %s", why)
	}
	r2()
	r3()
	if st := a.slots(snap); st.admitted != 0 || st.limit != 2 {
		t.Errorf("after releasing all: %+v", st)
	}
}
