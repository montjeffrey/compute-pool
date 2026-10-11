//go:build windows

package main

import (
	"strings"
	"syscall"
	"testing"
)

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
