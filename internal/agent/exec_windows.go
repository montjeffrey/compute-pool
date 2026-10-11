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

// lowerPriority is a no-op on Windows; CreationFlags already set the class.
func lowerPriority(*exec.Cmd) {}
