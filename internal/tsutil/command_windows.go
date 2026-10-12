//go:build windows

package tsutil

import (
	"context"
	"os/exec"
	"syscall"
)

const createNoWindow = 0x08000000

// command starts the tailscale CLI without a console window. The agent's
// startup task detaches from its console (--hide-console), so a console
// child would otherwise get a fresh one, which Windows 11 opens as a visible
// Terminal window every time the agent refreshes its peer list.
func command(ctx context.Context, bin string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow}
	return cmd
}
