//go:build !windows

package agent

import (
	"context"
	"os/exec"
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

// lowerPriority renices the job's process group so the owner's foreground
// work wins. Children started later inherit the niceness.
func lowerPriority(cmd *exec.Cmd) {
	_ = syscall.Setpriority(syscall.PRIO_PGRP, cmd.Process.Pid, 10)
}
