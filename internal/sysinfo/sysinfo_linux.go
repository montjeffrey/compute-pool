//go:build linux

package sysinfo

import (
	"os"
	"path/filepath"
	"strconv"
)

// TreeCPUTimes returns the CPU time used so far by this process and every
// live descendant (the agent and its jobs), in /proc/stat's units, so it can
// be compared with system-wide use.
func TreeCPUTimes() (uint64, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, err
	}
	procs := make([]procStat, 0, len(entries))
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		if err != nil {
			continue // exited while scanning
		}
		if p, err := parseProcPIDStat(string(b)); err == nil {
			procs = append(procs, p)
		}
	}
	return treeCPU(procs, os.Getpid()), nil
}

func readCPUTimes() (cpuTimes, error) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return cpuTimes{}, err
	}
	return parseProcStat(string(b))
}

func readMemory() (Memory, error) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return Memory{}, err
	}
	return parseMemInfo(string(b))
}

func readPower() (Power, error) {
	return readSysfsPower("/sys/class/power_supply")
}
