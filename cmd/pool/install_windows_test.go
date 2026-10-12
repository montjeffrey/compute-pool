//go:build windows

package main

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestPowerShellScriptsParse(t *testing.T) {
	const check = `
$errs = $null
[void][System.Management.Automation.Language.Parser]::ParseInput($env:POOL_TEST_SCRIPT, [ref]$null, [ref]$errs)
$errs | ForEach-Object { $_.Message }
`
	for name, script := range map[string]string{"stop": stopScript, "install": installScript, "uninstall": uninstallScript} {
		out, err := runPowerShell(context.Background(), check, map[string]string{"POOL_TEST_SCRIPT": script})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if out != "" {
			t.Errorf("%s script has syntax errors: %s", name, out)
		}
	}
}

func TestParseInstallOutput(t *testing.T) {
	mode, state, err := parseInstallOutput("at startup as PC\\me\r\nrunning")
	if err != nil || mode != `at startup as PC\me` || state != "running" {
		t.Errorf("got %q %q %v", mode, state, err)
	}
	for _, bad := range []string{"", "at logon as me", "at logon as me\nmaybe", "oops\nrunning"} {
		if _, _, err := parseInstallOutput(bad); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
}

// TestHelperProcess stands in for a running agent in TestStopScript.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("POOL_TEST_HELPER") != "1" {
		return
	}
	time.Sleep(time.Minute)
	os.Exit(0)
}

func startHelper(t *testing.T, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], append([]string{"-test.run=^TestHelperProcess$"}, args...)...)
	cmd.Env = append(os.Environ(), "POOL_TEST_HELPER=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return cmd
}

func TestStopScript(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	agent := startHelper(t, "agent", "--max-jobs", "2")
	installer := startHelper(t, "agent") // stands in for pool install run from the installed copy
	other := startHelper(t, "install", "--max-jobs", "2")

	exited := make(chan error, 1)
	go func() { exited <- agent.Wait() }()

	_, err = runPowerShell(context.Background(), stopScript, map[string]string{
		"POOL_TASK": "compute-pool test task that does not exist",
		"POOL_EXE":  self,
		"POOL_SELF": strconv.Itoa(installer.Process.Pid),
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the agent process was not stopped")
	}
	for name, cmd := range map[string]*exec.Cmd{"POOL_SELF": installer, "non-agent": other} {
		var code uint32
		h, err := syscall.OpenProcess(syscall.PROCESS_QUERY_INFORMATION, false, uint32(cmd.Process.Pid))
		if err != nil {
			t.Fatal(err)
		}
		err = syscall.GetExitCodeProcess(h, &code)
		syscall.CloseHandle(h)
		if err != nil {
			t.Fatal(err)
		}
		if code != 259 { // STILL_ACTIVE
			t.Errorf("the %s process was stopped", name)
		}
	}
}

func TestTaskCommandLineQuoting(t *testing.T) {
	line := taskCommandLine([]string{"--allow", "laptop,desktop", "--work-dir", `C:\Users\A B\jobs`}, `C:\Users\A B\agent.log`)
	want := `agent --allow laptop,desktop --work-dir "C:\Users\A B\jobs" --log-file "C:\Users\A B\agent.log" --hide-console`
	if line != want {
		t.Errorf("got  %s\nwant %s", line, want)
	}
	// Round-trip through the same parser Windows programs use.
	args, err := syscall.UTF16PtrFromString("pool.exe " + line)
	if err != nil {
		t.Fatal(err)
	}
	var argc int32
	argv, err := syscall.CommandLineToArgv(args, &argc)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for i := 1; i < int(argc); i++ {
		got = append(got, syscall.UTF16ToString(argv[i][:]))
	}
	if strings.Join(got, "|") != strings.Join([]string{"agent", "--allow", "laptop,desktop", "--work-dir", `C:\Users\A B\jobs`, "--log-file", `C:\Users\A B\agent.log`, "--hide-console"}, "|") {
		t.Errorf("parsed back as %q", got)
	}
}
