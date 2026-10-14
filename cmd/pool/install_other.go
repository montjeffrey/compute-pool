//go:build !windows

package main

import (
	"context"
	"errors"
	"io"
)

var errInstallUnsupported = errors.New("pool install supports Windows so far; elsewhere, run `pool agent --log-file ...` from a systemd user service or launchd agent (see README)")

// installedAgentLog is "" here: without pool install, the agent logs wherever
// its --log-file says.
func installedAgentLog() string { return "" }

func cmdInstall(context.Context, []string, io.Writer, io.Writer) error { return errInstallUnsupported }

func cmdUninstall(context.Context, []string, io.Writer, io.Writer) error {
	return errInstallUnsupported
}
