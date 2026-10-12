//go:build !windows

package tsutil

import (
	"context"
	"os/exec"
)

func command(ctx context.Context, bin string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, bin, args...)
}
