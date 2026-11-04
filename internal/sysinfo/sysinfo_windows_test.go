package sysinfo

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// TestHelperSpin is not a real test: TestJobCPUTimes runs this test binary
// as a CPU-bound child.
func TestHelperSpin(t *testing.T) {
	if os.Getenv("SYSINFO_SPIN") != "1" {
		return
	}
	n, end := 0, time.Now().Add(600*time.Millisecond)
	for time.Now().Before(end) {
		n++
	}
	os.Exit(n & 0) // 0, but keeps the loop from being optimized away
}

func TestJobCPUTimes(t *testing.T) {
	create := kernel32.NewProc("CreateJobObjectW")
	assign := kernel32.NewProc("AssignProcessToJobObject")
	job, _, err := create.Call(0, 0)
	if job == 0 {
		t.Fatalf("create job object: %v", err)
	}
	defer syscall.CloseHandle(syscall.Handle(job))
	if got, err := JobCPUTimes(job); err != nil || got != 0 {
		t.Fatalf("empty job: %d, %v", got, err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperSpin$")
	cmd.Env = append(os.Environ(), "SYSINFO_SPIN=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	const processSetQuota, processTerminate = 0x0100, 0x0001
	h, err := syscall.OpenProcess(processSetQuota|processTerminate, false, uint32(cmd.Process.Pid))
	if err != nil {
		cmd.Process.Kill()
		t.Fatal(err)
	}
	defer syscall.CloseHandle(h)
	if r, _, err := assign.Call(job, uintptr(h)); r == 0 {
		cmd.Process.Kill()
		t.Fatalf("assign to job: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("helper: %v", err)
	}
	got, err := JobCPUTimes(job)
	if err != nil {
		t.Fatal(err)
	}
	// The helper spun for 0.6s; allow for time used before it joined.
	if used := time.Duration(got) * 100; used < 300*time.Millisecond {
		t.Errorf("job CPU time = %v, want at least 300ms", used)
	}
}
