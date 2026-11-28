package agent

import (
	"syscall"
	"time"
)

// CanWatchExit reports whether WaitExited works on this platform.
const CanWatchExit = true

// WaitExited waits up to timeout for process pid to exit, without
// collecting its exit, and reports whether it did.
func WaitExited(pid int, timeout time.Duration) (bool, error) {
	h, err := syscall.OpenProcess(syscall.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return false, err
	}
	defer syscall.CloseHandle(h)
	ev, err := syscall.WaitForSingleObject(h, uint32(timeout.Milliseconds()))
	if err != nil {
		return false, err
	}
	return ev == syscall.WAIT_OBJECT_0, nil
}
