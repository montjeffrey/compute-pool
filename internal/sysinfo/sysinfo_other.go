//go:build !windows && !linux

package sysinfo

import (
	"os/exec"
	"runtime"
)

// CPU load needs cgo on macOS and the BSDs; report it as unknown there.
func readCPUTimes() (cpuTimes, error) {
	return cpuTimes{}, errUnsupported
}

// TreeCPUTimes is unknown here, like CPU load itself.
func TreeCPUTimes() (uint64, error) { return 0, errUnsupported }

func readPower() (Power, error) {
	if runtime.GOOS != "darwin" {
		return Power{}, errUnsupported
	}
	out, err := exec.Command("pmset", "-g", "batt").Output()
	if err != nil {
		return Power{}, err
	}
	return parsePmset(string(out)), nil
}
