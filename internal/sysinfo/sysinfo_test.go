package sysinfo

import (
	"errors"
	"math"
	"os"
	"path/filepath"
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
}
