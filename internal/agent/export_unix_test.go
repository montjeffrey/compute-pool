//go:build !windows

package agent

import (
	"bytes"
	"errors"
	"os"
	"runtime"
	"strconv"
	"time"
)

// CanWatchExit reports whether WaitExited works on this platform.
const CanWatchExit = runtime.GOOS == "linux"

// WaitExited waits up to timeout for process pid to exit, without
// collecting its exit (it is then a zombie), and reports whether it did.
func WaitExited(pid int, timeout time.Duration) (bool, error) {
	if !CanWatchExit {
		return false, errors.ErrUnsupported
	}
	for end := time.Now().Add(timeout); ; {
		stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil {
			return false, err
		}
		// The state follows the command name, which is in parentheses and
		// may itself hold spaces or parentheses.
		if i := bytes.LastIndexByte(stat, ')'); i >= 0 && i+2 < len(stat) && stat[i+2] == 'Z' {
			return true, nil
		}
		if time.Now().After(end) {
			return false, nil
		}
		time.Sleep(10 * time.Millisecond)
	}
}
