//go:build !windows

package main

import (
	"context"
	"errors"
	"io"
)

var errInstallUnsupported = errors.New("pool install supports Windows so far; elsewhere, run `pool agent --log-file ...` from a systemd user service or launchd agent (see README)")

// installedAgentLog is where pool setup tells the agent to log (there is no
// pool install here): the same folder as the saved measurement.
func installedAgentLog() string { return defaultAgentLog() }

func cmdInstall(context.Context, []string, io.Writer, io.Writer) error { return errInstallUnsupported }

func cmdUninstall(context.Context, []string, io.Writer, io.Writer) error {
	return errInstallUnsupported
}
