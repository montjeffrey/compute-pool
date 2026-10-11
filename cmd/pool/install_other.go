//go:build !windows

package main

import (
	"context"
	"errors"
	"io"
)

var errInstallUnsupported = errors.New("pool install supports Windows so far; elsewhere, run `pool agent --log-file ...` from a systemd user service or launchd agent (see README)")

func cmdInstall(context.Context, []string, io.Writer, io.Writer) error { return errInstallUnsupported }

func cmdUninstall(context.Context, []string, io.Writer, io.Writer) error {
	return errInstallUnsupported
}
