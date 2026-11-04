//go:build !windows

package agent

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"syscall"

	"github.com/montjeffrey/compute-pool/internal/proto"
)

// buildCommand prepares the job's process in its own process group so the
// whole tree can be killed and reniced together.
func buildCommand(ctx context.Context, req proto.RunRequest) *exec.Cmd {
	var cmd *exec.Cmd
	if req.Shell {
		cmd = exec.CommandContext(ctx, "/bin/sh", "-c", strings.Join(req.Argv, " "))
	} else {
		cmd = exec.CommandContext(ctx, req.Argv[0], req.Argv[1:]...)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	return cmd
}

// reapTree kills anything the job left running in its process group after
// the main process exited, so background children can't outlive the job,
// its slot accounting, or its scratch directory.
func reapTree(cmd *exec.Cmd) {
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

// lowerPriority renices the job's process group so the owner's foreground
// work wins. Children started later inherit the niceness.
func lowerPriority(cmd *exec.Cmd) {
	_ = syscall.Setpriority(syscall.PRIO_PGRP, cmd.Process.Pid, 10)
}

// trackMemory returns a function that reports the job's peak memory in bytes
// (0 if unknown) once it has exited: the largest resident size among the
// job's process and the descendants it waited for.
func trackMemory(cmd *exec.Cmd) func() uint64 {
	return func() uint64 {
		if cmd.ProcessState == nil {
			return 0
		}
		ru, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage)
		if !ok || ru.Maxrss <= 0 {
			return 0
		}
		if runtime.GOOS == "darwin" {
			return uint64(ru.Maxrss) // bytes on macOS
		}
		return uint64(ru.Maxrss) * 1024 // kilobytes elsewhere
	}
}
