//go:build !windows

package main

import (
	"os/exec"
	"runtime"

	"github.com/montjeffrey/compute-pool/internal/sysinfo"
)

// containChildren is a no-op here: each job runs in its own process group,
// which the agent kills on cancel, but a SIGKILLed agent leaves jobs running.
func containChildren() error { return nil }

func hideConsole() {}

// isElevated matters only to pool install, which is Windows-only.
func isElevated() bool { return false }

// ownCPUReader reads the CPU time of the agent and its jobs by walking the
// process tree (Linux; elsewhere it reports an error and stays unknown).
func ownCPUReader() func() (uint64, error) { return sysinfo.TreeCPUTimes }

func openBrowser(url string) error {
	opener := "xdg-open"
	if runtime.GOOS == "darwin" {
		opener = "open"
	}
	return exec.Command(opener, url).Start()
}
