// Package sysinfo measures how busy a device is and whether it is running on
// battery, so agents can decide whether to accept work.
package sysinfo

import (
	"bufio"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Power describes the device's power source.
type Power struct {
	Known      bool // false when the platform gave no answer
	HasBattery bool
	OnBattery  bool
	BatteryPct int // -1 when unknown
}

// Snapshot is the latest measurement.
type Snapshot struct {
	// CPUBusyPct is system-wide CPU use (0-100) over recent samples, or -1
	// before the first full interval or on platforms without support.
	CPUBusyPct float64
	Power      Power
}

// cpuTimes are cumulative counters in arbitrary but consistent units.
type cpuTimes struct {
	idle, total uint64
}

// busyPct returns the CPU use between two readings, or false if the
// counters did not advance.
func busyPct(prev, cur cpuTimes) (float64, bool) {
	if cur.total <= prev.total || cur.idle < prev.idle {
		return 0, false
	}
	dTotal := float64(cur.total - prev.total)
	dIdle := float64(cur.idle - prev.idle)
	if dIdle > dTotal {
		dIdle = dTotal
	}
	return 100 * (1 - dIdle/dTotal), true
}

// Sampler periodically measures CPU load and power state.
type Sampler struct {
	readCPU   func() (cpuTimes, error)
	readPower func() (Power, error)

	mu       sync.Mutex
	prev     cpuTimes
	havePrev bool
	busy     float64
	power    Power
}

// NewSampler returns a Sampler for this platform. Call Run to start it.
func NewSampler() *Sampler {
	s := &Sampler{readCPU: readCPUTimes, readPower: readPower, busy: -1}
	s.sample()
	return s
}

// Run samples every interval until ctx ends.
func (s *Sampler) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sample()
		}
	}
}

// Snapshot returns the latest measurement.
func (s *Sampler) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Snapshot{CPUBusyPct: s.busy, Power: s.power}
}

func (s *Sampler) sample() {
	cur, cpuErr := s.readCPU()
	pw, pwErr := s.readPower()

	s.mu.Lock()
	defer s.mu.Unlock()
	if cpuErr == nil {
		if s.havePrev {
			if pct, ok := busyPct(s.prev, cur); ok {
				// Smooth so one spike doesn't flip the agent between accepting
				// and refusing.
				if s.busy < 0 {
					s.busy = pct
				} else {
					s.busy = 0.5*s.busy + 0.5*pct
				}
			}
		}
		s.prev, s.havePrev = cur, true
	}
	if pwErr == nil {
		s.power = pw
	} else {
		s.power = Power{BatteryPct: -1}
	}
}

var errUnsupported = errors.New("not supported on this platform")

// parseProcStat reads the aggregate "cpu" line of Linux's /proc/stat.
func parseProcStat(data string) (cpuTimes, error) {
	sc := bufio.NewScanner(strings.NewReader(data))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 || fields[0] != "cpu" {
			continue
		}
		var t cpuTimes
		// user nice system idle iowait irq softirq steal; guest time is
		// already counted in user and nice.
		for i, f := range fields[1:] {
			if i >= 8 {
				break
			}
			v, err := strconv.ParseUint(f, 10, 64)
			if err != nil {
				return cpuTimes{}, err
			}
			t.total += v
			if i == 3 || i == 4 { // idle, iowait
				t.idle += v
			}
		}
		return t, nil
	}
	return cpuTimes{}, errors.New("no cpu line in /proc/stat")
}

// readSysfsPower inspects Linux /sys/class/power_supply (root is the
// directory, parameterized for tests).
func readSysfsPower(root string) (Power, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return Power{}, err
	}
	p := Power{Known: true, BatteryPct: -1}
	mainsSeen, mainsOnline, discharging := false, false, false
	read := func(dir, name string) string {
		b, _ := os.ReadFile(filepath.Join(root, dir, name))
		return strings.TrimSpace(string(b))
	}
	for _, e := range entries {
		switch read(e.Name(), "type") {
		case "Mains":
			mainsSeen = true
			if read(e.Name(), "online") == "1" {
				mainsOnline = true
			}
		case "Battery":
			p.HasBattery = true
			if read(e.Name(), "status") == "Discharging" {
				discharging = true
			}
			if pct, err := strconv.Atoi(read(e.Name(), "capacity")); err == nil && p.BatteryPct < 0 {
				p.BatteryPct = pct
			}
		}
	}
	if p.HasBattery {
		if mainsSeen {
			p.OnBattery = !mainsOnline
		} else {
			p.OnBattery = discharging
		}
	}
	return p, nil
}

// parsePmset reads the output of macOS `pmset -g batt`.
func parsePmset(out string) Power {
	p := Power{Known: true, BatteryPct: -1}
	p.OnBattery = strings.Contains(out, "'Battery Power'")
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "InternalBattery") {
			continue
		}
		p.HasBattery = true
		if i := strings.IndexByte(line, '%'); i > 0 {
			j := i
			for j > 0 && line[j-1] >= '0' && line[j-1] <= '9' {
				j--
			}
			if pct, err := strconv.Atoi(line[j:i]); err == nil {
				p.BatteryPct = pct
			}
		}
	}
	if !p.HasBattery {
		p.OnBattery = false
	}
	return p
}
