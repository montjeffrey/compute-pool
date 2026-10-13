package sysinfo

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestBusyPct(t *testing.T) {
	tests := []struct {
		prev, cur cpuTimes
		want      float64
		ok        bool
	}{
		{cpuTimes{idle: 100, total: 200}, cpuTimes{idle: 150, total: 300}, 50, true},
		{cpuTimes{idle: 0, total: 0}, cpuTimes{idle: 100, total: 100}, 0, true},
		{cpuTimes{idle: 0, total: 0}, cpuTimes{idle: 0, total: 100}, 100, true},
		{cpuTimes{idle: 10, total: 100}, cpuTimes{idle: 10, total: 100}, 0, false}, // no progress
		{cpuTimes{idle: 10, total: 100}, cpuTimes{idle: 5, total: 200}, 0, false},  // counter went backwards
	}
	for _, tt := range tests {
		got, ok := busyPct(tt.prev, tt.cur)
		if ok != tt.ok || math.Abs(got-tt.want) > 1e-9 {
			t.Errorf("busyPct(%+v, %+v) = %v, %v; want %v, %v", tt.prev, tt.cur, got, ok, tt.want, tt.ok)
		}
	}
}

func TestSamplerSmoothsAndReportsUnknownFirst(t *testing.T) {
	readings := []cpuTimes{
		{idle: 0, total: 0},
		{idle: 100, total: 100}, // 0% busy
		{idle: 100, total: 200}, // 100% busy
		{idle: 100, total: 300}, // 100% busy
	}
	i := 0
	s := &Sampler{
		busy: -1,
		readCPU: func() (cpuTimes, error) {
			r := readings[i]
			i++
			return r, nil
		},
		readPower: func() (Power, error) {
			return Power{Known: true, HasBattery: true, OnBattery: true, BatteryPct: 40}, nil
		},
	}
	s.sample()
	if got := s.Snapshot().CPUBusyPct; got != -1 {
		t.Fatalf("after one sample busy = %v, want -1 (unknown)", got)
	}
	want := []float64{0, 50, 75}
	for _, w := range want {
		s.sample()
		if got := s.Snapshot().CPUBusyPct; math.Abs(got-w) > 1e-9 {
			t.Errorf("busy = %v, want %v", got, w)
		}
	}
	if p := s.Snapshot().Power; !p.OnBattery || p.BatteryPct != 40 {
		t.Errorf("power = %+v", p)
	}
}

func TestSamplerSeparatesOwnUse(t *testing.T) {
	cpu := []cpuTimes{{idle: 0, total: 0}, {idle: 20, total: 100}, {idle: 30, total: 200}}
	own := []uint64{1000, 1060, 1130} // 60 then 70 of each 100 units
	i := 0
	s := &Sampler{
		busy:      -1,
		readCPU:   func() (cpuTimes, error) { return cpu[i], nil },
		readPower: func() (Power, error) { return Power{}, nil },
	}
	s.SetOwnReader(func() (uint64, error) { r := own[i]; i++; return r, nil })
	s.sample()
	if snap := s.Snapshot(); snap.OwnBusyPct != -1 || snap.OtherBusyPct() != -1 {
		t.Fatalf("before a full interval: %+v", snap)
	}
	s.sample() // 80% busy, 60% ours
	if snap := s.Snapshot(); snap.CPUBusyPct != 80 || snap.OwnBusyPct != 60 || snap.OtherBusyPct() != 20 {
		t.Errorf("first interval: %+v, other %v", snap, snap.OtherBusyPct())
	}
	s.sample() // 90% busy, 70% ours; smoothed: 85 and 65
	if snap := s.Snapshot(); snap.CPUBusyPct != 85 || snap.OwnBusyPct != 65 || snap.OtherBusyPct() != 20 {
		t.Errorf("second interval: %+v, other %v", snap, snap.OtherBusyPct())
	}
}

func TestSamplerOwnUseUnknownCountsEverything(t *testing.T) {
	cpu := []cpuTimes{{idle: 0, total: 0}, {idle: 20, total: 100}}
	i := 0
	s := &Sampler{
		busy:      -1,
		readCPU:   func() (cpuTimes, error) { r := cpu[i]; i++; return r, nil },
		readPower: func() (Power, error) { return Power{}, nil },
	}
	s.SetOwnReader(func() (uint64, error) { return 0, errUnsupported })
	s.sample()
	s.sample()
	if snap := s.Snapshot(); snap.CPUBusyPct != 80 || snap.OwnBusyPct != -1 || snap.OtherBusyPct() != 80 {
		t.Errorf("snapshot = %+v, other %v", snap, snap.OtherBusyPct())
	}
	// Our share can't exceed the total, whatever the counters say.
	if got := (Snapshot{CPUBusyPct: 30, OwnBusyPct: 50}).OtherBusyPct(); got != 0 {
		t.Errorf("OtherBusyPct with own above total = %v", got)
	}
}

func TestParseProcPIDStat(t *testing.T) {
	line := "4321 (pool (agent) x) S 1 4321 4321 0 -1 4194560 100 0 0 0 250 50 30 20 20 0 9 0 12345 0 0\n"
	p, err := parseProcPIDStat(line)
	if err != nil || p.pid != 4321 || p.ppid != 1 || p.cpu != 350 {
		t.Errorf("parse = %+v, %v", p, err)
	}
	for _, bad := range []string{"", "12 no-parens S 1", "x (a) S 1 2 3", "1 (a) S 1 2 3 4 5 6 7 8 9 10 x 1 1 1"} {
		if _, err := parseProcPIDStat(bad); err == nil {
			t.Errorf("parseProcPIDStat(%q): no error", bad)
		}
	}
}

func TestTreeCPU(t *testing.T) {
	procs := []procStat{
		{pid: 1, ppid: 0, cpu: 1000},  // init: not ours
		{pid: 10, ppid: 1, cpu: 5},    // the agent
		{pid: 11, ppid: 10, cpu: 300}, // a job
		{pid: 12, ppid: 11, cpu: 200}, // its child
		{pid: 13, ppid: 10, cpu: 100}, // another job
		{pid: 20, ppid: 1, cpu: 900},  // the owner's program
	}
	if got := treeCPU(procs, 10); got != 605 {
		t.Errorf("treeCPU = %d, want 605", got)
	}
	if got := treeCPU(procs, 99); got != 0 {
		t.Errorf("treeCPU of a missing process = %d", got)
	}
}

func TestSamplerPowerErrorMeansUnknown(t *testing.T) {
	s := &Sampler{
		busy:      -1,
		readCPU:   func() (cpuTimes, error) { return cpuTimes{}, errUnsupported },
		readPower: func() (Power, error) { return Power{}, errors.New("boom") },
	}
	s.sample()
	snap := s.Snapshot()
	if snap.CPUBusyPct != -1 || snap.Power.Known || snap.Power.OnBattery || snap.Power.BatteryPct != -1 {
		t.Errorf("snapshot = %+v", snap)
	}
}

func TestParseProcStat(t *testing.T) {
	data := "cpu  4705 150 1120 16250 520 0 30 5 70 0\ncpu0 1 2 3 4 5 6 7 8 0 0\nintr 1 2 3\n"
	got, err := parseProcStat(data)
	if err != nil {
		t.Fatal(err)
	}
	want := cpuTimes{idle: 16250 + 520, total: 4705 + 150 + 1120 + 16250 + 520 + 0 + 30 + 5}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if _, err := parseProcStat("intr 1 2\n"); err == nil {
		t.Error("want error without a cpu line")
	}
}

func writeSupply(t *testing.T, root, name string, files map[string]string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for k, v := range files {
		if err := os.WriteFile(filepath.Join(dir, k), []byte(v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReadSysfsPower(t *testing.T) {
	t.Run("laptop unplugged", func(t *testing.T) {
		root := t.TempDir()
		writeSupply(t, root, "AC", map[string]string{"type": "Mains", "online": "0"})
		writeSupply(t, root, "BAT0", map[string]string{"type": "Battery", "status": "Discharging", "capacity": "54"})
		p, err := readSysfsPower(root)
		if err != nil {
			t.Fatal(err)
		}
		if !p.HasBattery || !p.OnBattery || p.BatteryPct != 54 {
			t.Errorf("got %+v", p)
		}
	})
	t.Run("laptop plugged in", func(t *testing.T) {
		root := t.TempDir()
		writeSupply(t, root, "AC", map[string]string{"type": "Mains", "online": "1"})
		writeSupply(t, root, "BAT0", map[string]string{"type": "Battery", "status": "Charging", "capacity": "80"})
		p, _ := readSysfsPower(root)
		if !p.HasBattery || p.OnBattery {
			t.Errorf("got %+v", p)
		}
	})
	t.Run("battery only, no mains entry", func(t *testing.T) {
		root := t.TempDir()
		writeSupply(t, root, "BAT1", map[string]string{"type": "Battery", "status": "Discharging"})
		p, _ := readSysfsPower(root)
		if !p.OnBattery || p.BatteryPct != -1 {
			t.Errorf("got %+v", p)
		}
	})
	t.Run("desktop", func(t *testing.T) {
		p, err := readSysfsPower(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if p.HasBattery || p.OnBattery {
			t.Errorf("got %+v", p)
		}
	})
}

func TestParsePmset(t *testing.T) {
	onBattery := "Now drawing from 'Battery Power'\n -InternalBattery-0 (id=1234567)\t62%; discharging; 4:10 remaining present: true\n"
	p := parsePmset(onBattery)
	if !p.HasBattery || !p.OnBattery || p.BatteryPct != 62 {
		t.Errorf("on battery: got %+v", p)
	}
	charging := "Now drawing from 'AC Power'\n -InternalBattery-0 (id=1234567)\t100%; charged; 0:00 remaining present: true\n"
	p = parsePmset(charging)
	if !p.HasBattery || p.OnBattery || p.BatteryPct != 100 {
		t.Errorf("charging: got %+v", p)
	}
	p = parsePmset("Now drawing from 'AC Power'\n")
	if p.HasBattery || p.OnBattery {
		t.Errorf("desktop Mac: got %+v", p)
	}
}

func TestParseMemInfo(t *testing.T) {
	data := "MemTotal:       16303428 kB\nMemFree:          812344 kB\nMemAvailable:    9437184 kB\nBuffers: 1 kB\n"
	m, err := parseMemInfo(data)
	if err != nil || m != (Memory{Known: true, TotalMB: 15921, FreeMB: 9216}) {
		t.Errorf("parse = %+v, %v", m, err)
	}
	// Kernels before 3.14 have no MemAvailable; don't guess from MemFree.
	if _, err := parseMemInfo("MemTotal: 1000 kB\nMemFree: 500 kB\n"); err == nil {
		t.Error("no error without MemAvailable")
	}
}

func TestSamplerReportsMemory(t *testing.T) {
	free := int64(4000)
	s := &Sampler{
		busy:      -1,
		readCPU:   func() (cpuTimes, error) { return cpuTimes{}, errUnsupported },
		readPower: func() (Power, error) { return Power{}, nil },
		readMem:   func() (Memory, error) { return Memory{Known: true, TotalMB: 8000, FreeMB: free}, nil },
	}
	s.sample()
	free = 1500 // not smoothed: the latest reading counts
	s.sample()
	if m := s.Snapshot().Memory; !m.Known || m.FreeMB != 1500 || m.TotalMB != 8000 {
		t.Errorf("memory = %+v", m)
	}
	s.readMem = func() (Memory, error) { return Memory{}, errUnsupported }
	s.sample()
	if m := s.Snapshot().Memory; m.Known {
		t.Errorf("after a failed read memory = %+v, want unknown", m)
	}
}

func TestRealSamplerDoesNotFail(t *testing.T) {
	// Smoke test against this machine: values may be unknown but must be sane.
	s := NewSampler()
	s.sample()
	snap := s.Snapshot()
	if snap.CPUBusyPct != -1 && (snap.CPUBusyPct < 0 || snap.CPUBusyPct > 100) {
		t.Errorf("CPUBusyPct = %v", snap.CPUBusyPct)
	}
	if snap.Power.BatteryPct < -1 || snap.Power.BatteryPct > 100 {
		t.Errorf("BatteryPct = %v", snap.Power.BatteryPct)
	}
	if m := snap.Memory; m.Known && (m.TotalMB <= 0 || m.FreeMB < 0 || m.FreeMB > m.TotalMB) {
		t.Errorf("Memory = %+v", m)
	}
	if (runtime.GOOS == "windows" || runtime.GOOS == "linux") && !snap.Memory.Known {
		t.Error("memory unknown on a platform that supports it")
	}
}
