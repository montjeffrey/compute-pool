//go:build windows

package agent

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"unsafe"

	"github.com/montjeffrey/compute-pool/internal/proto"
	"github.com/montjeffrey/compute-pool/internal/sysinfo"
)

const (
	belowNormalPriorityClass = 0x00004000
	createNoWindow           = 0x08000000
)

// buildCommand prepares the job's process. Jobs start at below-normal
// priority (inherited by their children) so the owner's foreground work wins,
// in their own process group so the agent's Ctrl+C does not reach them, and
// without a console window so nothing pops up on the lending machine.
func buildCommand(ctx context.Context, req proto.RunRequest) *exec.Cmd {
	var cmd *exec.Cmd
	attr := &syscall.SysProcAttr{}
	if req.Shell {
		comspec := os.Getenv("ComSpec")
		if comspec == "" {
			comspec = "cmd.exe"
		}
		cmd = exec.CommandContext(ctx, comspec)
		// CmdLine replaces the whole command line, so it repeats the program.
		// /S makes cmd strip exactly the outer quotes and keep the rest as typed.
		attr.CmdLine = `"` + comspec + `" /S /C "` + strings.Join(req.Argv, " ") + `"`
	} else {
		cmd = exec.CommandContext(ctx, req.Argv[0], req.Argv[1:]...)
	}
	attr.CreationFlags |= belowNormalPriorityClass | syscall.CREATE_NEW_PROCESS_GROUP | createNoWindow
	cmd.SysProcAttr = attr
	cmd.Cancel = func() error { return killTree(cmd) }
	return cmd
}

// stoppedExitCode is the exit code killTree gives the processes it ends,
// so the agent can tell from a job's exit that it ended the job: taskkill
// /F and Process.Kill leave 1, which programs also exit with themselves.
const stoppedExitCode = 0x504F4F4C // "POOL"

// killTree ends the job's process and every process it started, with
// stoppedExitCode. Process.Kill alone would leave grandchildren (compilers,
// test runners) running. The process itself goes first, so it can't start
// more; its descendants are found by parent first, as taskkill /T finds
// them, and ended even if it had exited already. It returns
// os.ErrProcessDone if the process had exited before it could be ended.
func killTree(cmd *exec.Cmd) error {
	pid := uint32(cmd.Process.Pid)
	tree := descendants(pid)
	err := terminate(pid)
	for _, p := range tree {
		_ = terminate(p) // it may have exited already
	}
	return err
}

func terminate(pid uint32) error {
	h, err := syscall.OpenProcess(syscall.PROCESS_TERMINATE|syscall.SYNCHRONIZE, false, pid)
	if err != nil {
		return err
	}
	defer syscall.CloseHandle(h)
	if err := syscall.TerminateProcess(h, stoppedExitCode); err != nil {
		if ev, _ := syscall.WaitForSingleObject(h, 0); ev == syscall.WAIT_OBJECT_0 {
			return os.ErrProcessDone // it exited on its own first
		}
		return err
	}
	return nil
}

// descendants lists the processes started by pid, and by those, from a
// snapshot of the processes running now; nil if there is none.
func descendants(pid uint32) []uint32 {
	snap, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer syscall.CloseHandle(snap)
	children := map[uint32][]uint32{}
	e := syscall.ProcessEntry32{Size: uint32(unsafe.Sizeof(syscall.ProcessEntry32{}))}
	for err := syscall.Process32First(snap, &e); err == nil; err = syscall.Process32Next(snap, &e) {
		if e.ProcessID != e.ParentProcessID { // the idle process is its own parent
			children[e.ParentProcessID] = append(children[e.ParentProcessID], e.ProcessID)
		}
	}
	var tree []uint32
	seen := map[uint32]bool{pid: true}
	for queue := []uint32{pid}; len(queue) > 0; queue = queue[1:] {
		for _, c := range children[queue[0]] {
			if !seen[c] {
				seen[c] = true
				tree = append(tree, c)
				queue = append(queue, c)
			}
		}
	}
	return tree
}

// killedByStop reports whether the job's process ended the way Cancel ends
// it (killTree, with stoppedExitCode), not by exiting on its own. A process
// never waited for (state nil) counts as killed.
func killedByStop(state *os.ProcessState) bool {
	return state == nil || state.ExitCode() == stoppedExitCode
}

// reapTree is a no-op on Windows: once the main process has exited its
// children can no longer be found by parent. The job's own job object ends
// them instead when trackMemory's function releases it, and the agent-wide
// job object ends any that escaped tracking when the agent exits.
func reapTree(*exec.Cmd) {}

// lowerPriority is a no-op on Windows; CreationFlags already set the class.
func lowerPriority(*exec.Cmd) {}

// JobMemoryLimitEnforced reports whether Config.JobMemoryLimitMB takes
// effect on this platform.
const JobMemoryLimitEnforced = true

// trackMemory starts measuring the job's memory and, if limitMB is above
// zero, ends the job and its children as soon as they commit more than that
// together, calling onLimit first. capped reports whether the limit was set.
// The returned function reports the peak in bytes (0 if unknown) once the
// job has exited. A child the job starts in the instant before tracking
// begins isn't counted or held to the limit, but the limit still ends it:
// killTree runs before the job object ends the job's process, while that
// child can still be found by its parent.
func trackMemory(cmd *exec.Cmd, limitMB int64, onLimit func()) (peak func() uint64, capped bool) {
	return sysinfo.TrackMemory(cmd.Process.Pid, uint64(max(0, limitMB))<<20, func() {
		onLimit()
		_ = killTree(cmd)
	})
}
