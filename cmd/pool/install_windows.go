//go:build windows

package main

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
)

const taskName = "compute-pool agent"

type installPaths struct {
	base, bin, exe, log string
}

func defaultInstallPaths() (installPaths, error) {
	local := os.Getenv("LOCALAPPDATA")
	if local == "" {
		return installPaths{}, errors.New("LOCALAPPDATA is not set")
	}
	base := filepath.Join(local, "compute-pool")
	return installPaths{
		base: base,
		bin:  filepath.Join(base, "bin"),
		exe:  filepath.Join(base, "bin", "pool.exe"),
		log:  filepath.Join(base, "agent.log"),
	}, nil
}

// taskCommandLine builds the startup task's arguments. Each argument is
// quoted the way Windows programs parse them.
func taskCommandLine(agentArgs []string, logPath string) string {
	args := append([]string{"agent"}, agentArgs...)
	args = append(args, "--log-file", logPath, "--hide-console")
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = syscall.EscapeArg(a)
	}
	return strings.Join(quoted, " ")
}

// The scripts read their inputs from environment variables so no value is
// ever spliced into PowerShell source. Progress output is silenced because
// a non-interactive PowerShell serializes it onto stderr.
const stopScript = `
$ProgressPreference = 'SilentlyContinue'
$t = Get-ScheduledTask -TaskName $env:POOL_TASK -ErrorAction SilentlyContinue
if ($t) { Stop-ScheduledTask -TaskName $env:POOL_TASK -ErrorAction SilentlyContinue }
Get-Process -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq $env:POOL_EXE } | Stop-Process -Force -ErrorAction SilentlyContinue
`

// installScript prefers an S4U task (runs at boot, before anyone logs in,
// with no window), which needs an elevated shell, and otherwise falls back
// to running at the user's logon. Both run as the current user, never SYSTEM,
// and neither stores a password.
//
// Task Scheduler's restart-on-failure only covers a task that fails to
// launch, not a program that later exits or is killed, so a trigger that
// fires every minute acts as the watchdog: with MultipleInstances IgnoreNew
// it does nothing while the agent runs and relaunches it if it has died.
const installScript = `
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
# USERDOMAIN reads "WORKGROUP" in SSH sessions; the token's name is reliable.
$user = [Security.Principal.WindowsIdentity]::GetCurrent().Name
$action = New-ScheduledTaskAction -Execute $env:POOL_EXE -Argument $env:POOL_ARGS -WorkingDirectory $env:POOL_DIR
$settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -DontStopOnIdleEnd -StartWhenAvailable -ExecutionTimeLimit ([TimeSpan]::Zero) -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1) -MultipleInstances IgnoreNew
$watchdog = New-ScheduledTaskTrigger -Once -At (Get-Date).AddMinutes(1) -RepetitionInterval (New-TimeSpan -Minutes 1)
$mode = 'at startup'
try {
  $principal = New-ScheduledTaskPrincipal -UserId $user -LogonType S4U -RunLevel Limited
  $triggers = @((New-ScheduledTaskTrigger -AtStartup), (New-ScheduledTaskTrigger -AtLogOn -User $user), $watchdog)
  Register-ScheduledTask -TaskName $env:POOL_TASK -Action $action -Trigger $triggers -Settings $settings -Principal $principal -Force | Out-Null
} catch {
  $mode = 'at logon'
  $principal = New-ScheduledTaskPrincipal -UserId $user -LogonType Interactive -RunLevel Limited
  $triggers = @((New-ScheduledTaskTrigger -AtLogOn -User $user), $watchdog)
  Register-ScheduledTask -TaskName $env:POOL_TASK -Action $action -Trigger $triggers -Settings $settings -Principal $principal -Force | Out-Null
}
Start-ScheduledTask -TaskName $env:POOL_TASK
"$mode as $user"
`

const uninstallScript = stopScript + `
$t = Get-ScheduledTask -TaskName $env:POOL_TASK -ErrorAction SilentlyContinue
if ($t) { Unregister-ScheduledTask -TaskName $env:POOL_TASK -Confirm:$false; 'removed' } else { 'not installed' }
`

func runPowerShell(ctx context.Context, script string, env map[string]string) (string, error) {
	// -EncodedCommand runs the script as one unit; piping it to "-Command -"
	// would run it line by line and silently break multi-line blocks.
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
		"-EncodedCommand", encodePowerShell(script))
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	text := strings.TrimSpace(string(out))
	if err != nil {
		return text, fmt.Errorf("%w: %s %s", err, text, strings.TrimSpace(stderr.String()))
	}
	return text, nil
}

// encodePowerShell encodes a script for -EncodedCommand: base64 of UTF-16LE.
func encodePowerShell(script string) string {
	u := utf16.Encode([]rune(script))
	b := make([]byte, 2*len(u))
	for i, c := range u {
		binary.LittleEndian.PutUint16(b[2*i:], c)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func copyExecutable(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	// The old agent may take a moment to release the file after stopping.
	var werr error
	for i := 0; i < 10; i++ {
		if werr = os.WriteFile(dst, data, 0o755); werr == nil {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return werr
}

func cmdInstall(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs, _ := newAgentFlags("install", stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: pool install [agent flags]   (the same flags as pool agent)")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	p, err := defaultInstallPaths()
	if err != nil {
		return err
	}
	env := map[string]string{"POOL_TASK": taskName, "POOL_EXE": p.exe, "POOL_DIR": p.base}

	// Stop any running copy first so its executable can be replaced.
	if _, err := runPowerShell(ctx, stopScript, env); err != nil {
		return fmt.Errorf("stop existing agent: %w", err)
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if !strings.EqualFold(filepath.Clean(self), filepath.Clean(p.exe)) {
		if err := copyExecutable(self, p.exe); err != nil {
			return fmt.Errorf("copy pool.exe to %s: %w", p.bin, err)
		}
	}

	env["POOL_ARGS"] = taskCommandLine(args, p.log)
	mode, err := runPowerShell(ctx, installScript, env)
	if err != nil {
		return fmt.Errorf("register startup task: %w", err)
	}
	if !strings.HasPrefix(mode, "at ") {
		return fmt.Errorf("register startup task: unexpected output %q", mode)
	}
	fmt.Fprintf(stdout, "Installed: the agent now runs %s.\n", mode)
	fmt.Fprintf(stdout, "  program: %s\n  log:     %s\n", p.exe, p.log)
	fmt.Fprintf(stdout, "It restarts within a minute if it stops. Remove it with: pool uninstall\n")
	return nil
}

func cmdUninstall(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) > 0 {
		return fmt.Errorf("unexpected argument %q", args[0])
	}
	p, err := defaultInstallPaths()
	if err != nil {
		return err
	}
	out, err := runPowerShell(ctx, uninstallScript, map[string]string{"POOL_TASK": taskName, "POOL_EXE": p.exe})
	if err != nil {
		return err
	}
	self, _ := os.Executable()
	if !strings.EqualFold(filepath.Clean(self), filepath.Clean(p.exe)) {
		_ = os.RemoveAll(p.bin)
	}
	fmt.Fprintf(stdout, "Startup task %s. Logs are kept in %s.\n", out, p.base)
	return nil
}
