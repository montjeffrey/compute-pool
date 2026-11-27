//go:build windows

package agent

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"github.com/montjeffrey/compute-pool/internal/proto"
	"github.com/montjeffrey/compute-pool/internal/sysinfo"
)

const (
	belowNormalPriorityClass = 0x00004000
	createNoWindow           = 0x08000000
)

// buildCommand prepares the job's process. Jobs start at below-normal
// priority (inherited by their children) so the owner's foreground work wins,
// in their own process group so the agent's Ctrl+C does not reach them, and
// without a console window so nothing pops up on the lending machine.
func buildCommand(ctx context.Context, req proto.RunRequest) *exec.Cmd {
	var cmd *exec.Cmd
	attr := &syscall.SysProcAttr{}
	if req.Shell {
		comspec := os.Getenv("ComSpec")
		if comspec == "" {
			comspec = "cmd.exe"
		}
		cmd = exec.CommandContext(ctx, comspec)
		// CmdLine replaces the whole command line, so it repeats the program.
		// /S makes cmd strip exactly the outer quotes and keep the rest as typed.
		attr.CmdLine = `"` + comspec + `" /S /C "` + strings.Join(req.Argv, " ") + `"`
	} else {
		cmd = exec.CommandContext(ctx, req.Argv[0], req.Argv[1:]...)
	}
	attr.CreationFlags |= belowNormalPriorityClass | syscall.CREATE_NEW_PROCESS_GROUP | createNoWindow
	cmd.SysProcAttr = attr
	cmd.Cancel = func() error { return killTree(cmd) }
	return cmd
}

// killTree ends the job and every process it started. Process.Kill alone
// would leave grandchildren (compilers, test runners) running.
func killTree(cmd *exec.Cmd) error {
	kill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
	kill.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow}
	if err := kill.Run(); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}

// reapTree is a no-op on Windows: once the main process has exited its
// children can no longer be found by parent. The job's own job object ends
// them instead when trackMemory's function releases it, and the agent-wide
// job object ends any that escaped tracking when the agent exits.
func reapTree(*exec.Cmd) {}

// lowerPriority is a no-op on Windows; CreationFlags already set the class.
func lowerPriority(*exec.Cmd) {}

// JobMemoryLimitEnforced reports whether Config.JobMemoryLimitMB takes
// effect on this platform.
const JobMemoryLimitEnforced = true

// trackMemory starts measuring the job's memory and, if limitMB is above
// zero, caps what the job and its children can commit together, calling
// onLimit once as they reach it. capped reports whether the cap was set
// (sysinfo.TrackMemory explains when it isn't). The returned function
// reports the peak in bytes (0 if unknown) once the job has exited. A child
// the job starts in the instant before tracking begins isn't counted or
// capped.
func trackMemory(cmd *exec.Cmd, limitMB int64, onLimit func()) (peak func() uint64, capped bool) {
	return sysinfo.TrackMemory(cmd.Process.Pid, uint64(max(0, limitMB))<<20, onLimit)
}
