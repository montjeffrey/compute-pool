//go:build windows

package tsutil

import (
	"context"
	"testing"
)

func TestCommandHasNoConsoleWindow(t *testing.T) {
	cmd := command(context.Background(), "tailscale.exe", "status", "--json")
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.CreationFlags&createNoWindow == 0 {
		t.Fatalf("tailscale must start with CREATE_NO_WINDOW, got SysProcAttr %+v", cmd.SysProcAttr)
	}
}
