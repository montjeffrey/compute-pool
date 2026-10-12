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
	// OwnBusyPct is the part of CPUBusyPct used by this process and its
	// descendants (the agent and its jobs), or -1 when unknown.
	OwnBusyPct float64
	Power      Power
}

// OtherBusyPct is CPU use by everything except this process and its
// descendants: the owner's own work. It is CPUBusyPct when OwnBusyPct is
// unknown, and -1 when CPU use is unknown.
func (s Snapshot) OtherBusyPct() float64 {
	if s.CPUBusyPct < 0 || s.OwnBusyPct < 0 {
		return s.CPUBusyPct
	}
	return max(0, s.CPUBusyPct-s.OwnBusyPct)
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
	// readOwn reports the CPU time used by this process and its
	// descendants, in the same units as readCPU's totals; nil if unknown.
	readOwn func() (uint64, error)

	mu       sync.Mutex
	prev     cpuTimes
	havePrev bool
	busy     float64
	power    Power

	prevOwn  uint64
	haveOwn  bool
	own      float64
	ownKnown bool
}

// NewSampler returns a Sampler for this platform. Call Run to start it.
func NewSampler() *Sampler {
	s := &Sampler{readCPU: readCPUTimes, readPower: readPower, busy: -1}
	s.sample()
	return s
}

// SetOwnReader tells the sampler how to read the CPU time used by this
// process and its descendants, in the units of the system-wide counters
// (see JobCPUTimes and TreeCPUTimes). Call it before Run.
func (s *Sampler) SetOwnReader(read func() (uint64, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.readOwn, s.haveOwn, s.ownKnown = read, false, false
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
	own := -1.0
	if s.ownKnown && s.busy >= 0 {
		own = min(s.own, s.busy)
	}
	return Snapshot{CPUBusyPct: s.busy, OwnBusyPct: own, Power: s.power}
}

// smooth blends a new reading into a running value so one spike doesn't
// flip the agent between accepting and refusing.
func smooth(old, cur float64, known bool) float64 {
	if !known {
		return cur
	}
	return 0.5*old + 0.5*cur
}

func (s *Sampler) sample() {
	s.mu.Lock()
	readOwn := s.readOwn
	s.mu.Unlock()
	// Read the counters back to back so they cover the same interval.
	cur, cpuErr := s.readCPU()
	var own uint64
	ownErr := errUnsupported
	if readOwn != nil {
		own, ownErr = readOwn()
	}
	pw, pwErr := s.readPower()

	s.mu.Lock()
	defer s.mu.Unlock()
	if cpuErr == nil {
		if s.havePrev {
			if pct, ok := busyPct(s.prev, cur); ok {
				s.busy = smooth(s.busy, pct, s.busy >= 0)
				if ownErr == nil && s.haveOwn && own >= s.prevOwn {
					ownPct := min(100, 100*float64(own-s.prevOwn)/float64(cur.total-s.prev.total))
					s.own = smooth(s.own, ownPct, s.ownKnown)
					s.ownKnown = true
				}
			}
		}
		s.prev, s.havePrev = cur, true
	}
	if ownErr == nil {
		s.prevOwn, s.haveOwn = own, true
	} else {
		s.haveOwn, s.ownKnown = false, false
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

// procStat is the part of a Linux /proc/<pid>/stat line that matters here.
type procStat struct {
	pid, ppid int
	// cpu is the process's user and system time plus that of its children
	// it has waited for, in clock ticks (the units of /proc/stat).
	cpu uint64
}

// parseProcPIDStat reads a /proc/<pid>/stat line. The command name is in
// parentheses and may itself contain spaces and parentheses, so fields are
// counted from the last ')'.
func parseProcPIDStat(line string) (procStat, error) {
	open, end := strings.IndexByte(line, '('), strings.LastIndexByte(line, ')')
	if open < 0 || end < open {
		return procStat{}, errors.New("malformed stat line")
	}
	pid, err := strconv.Atoi(strings.TrimSpace(line[:open]))
	if err != nil {
		return procStat{}, err
	}
	// After the name: state ppid pgrp session tty tpgid flags minflt
	// cminflt majflt cmajflt utime stime cutime cstime ...
	f := strings.Fields(line[end+1:])
	if len(f) < 15 {
		return procStat{}, errors.New("short stat line")
	}
	ppid, err := strconv.Atoi(f[1])
	if err != nil {
		return procStat{}, err
	}
	var cpu uint64
	for _, s := range f[11:15] {
		v, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return procStat{}, err
		}
		cpu += v
	}
	return procStat{pid: pid, ppid: ppid, cpu: cpu}, nil
}

// treeCPU sums the CPU time of root and every live descendant. Descendants
// that have exited are already counted in their parent's waited-for time.
func treeCPU(procs []procStat, root int) uint64 {
	children := map[int][]int{}
	cpu := map[int]uint64{}
	for _, p := range procs {
		children[p.ppid] = append(children[p.ppid], p.pid)
		cpu[p.pid] = p.cpu
	}
	var total uint64
	seen := map[int]bool{}
	stack := []int{root}
	for len(stack) > 0 {
		pid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		total += cpu[pid]
		stack = append(stack, children[pid]...)
	}
	return total
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
