//go:build linux

package sysinfo

import "os"

func readCPUTimes() (cpuTimes, error) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return cpuTimes{}, err
	}
	return parseProcStat(string(b))
}

func readPower() (Power, error) {
	return readSysfsPower("/sys/class/power_supply")
}
