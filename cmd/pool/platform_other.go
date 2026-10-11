//go:build !windows

package main

import (
	"os/exec"
	"runtime"
)

// containChildren is a no-op here: each job runs in its own process group,
// which the agent kills on cancel, but a SIGKILLed agent leaves jobs running.
func containChildren() error { return nil }

func hideConsole() {}

func openBrowser(url string) error {
	opener := "xdg-open"
	if runtime.GOOS == "darwin" {
		opener = "open"
	}
	return exec.Command(opener, url).Start()
}
