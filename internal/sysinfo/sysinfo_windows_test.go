package sysinfo

import (
	"os"
	"os/exec"
	"sync"
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

// TestHelperGrow is not a real test: TestTrackMemoryLimit runs this test
// binary as a child that uses more and more memory, then waits.
func TestHelperGrow(t *testing.T) {
	var keep [][]byte
	switch os.Getenv("SYSINFO_GROW") {
	case "steps": // 4 MB at a time up to 1 GB, touching each page
		for i := 0; i < 256; i++ {
			b := make([]byte, 4<<20)
			for j := 0; j < len(b); j += 4096 {
				b[j] = 1
			}
			keep = append(keep, b)
		}
	case "jump": // 1 GB in one allocation, far past the limit
		keep = append(keep, make([]byte, 1<<30))
	default:
		return
	}
	time.Sleep(time.Minute) // until the limit ends it
	os.Exit(len(keep) & 0)
}

func TestTrackMemoryLimit(t *testing.T) {
	for _, mode := range []string{"steps", "jump"} {
		t.Run(mode, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestHelperGrow$")
			cmd.Env = append(os.Environ(), "SYSINFO_GROW="+mode)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer cmd.Process.Kill()
			reached := make(chan struct{})
			const limit = 128 << 20
			track, capped := TrackMemory(cmd.Process.Pid, limit, func() { close(reached) }) // once only, or this panics
			peak := sync.OnceValue(track)                                                   // releases the job once
			defer peak()
			if !capped {
				t.Fatal("TrackMemory didn't set the limit")
			}
			exited := make(chan error, 1)
			go func() { exited <- cmd.Wait() }()
			select {
			case err := <-exited:
				if err == nil {
					t.Error("the job exited cleanly; want it ended by its limit")
				}
			case <-time.After(30 * time.Second):
				t.Fatal("the job wasn't ended within 30s of going past its limit")
			}
			select {
			case <-reached:
			default:
				t.Error("onLimit wasn't called")
			}
			if got := peak(); got <= limit {
				t.Errorf("peak = %d bytes, want past the %d-byte limit", got, limit)
			}
		})
	}
}
