package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync/atomic"
	"time"

	"github.com/montjeffrey/compute-pool/internal/proto"
)

// Exit codes reported when the agent, not the program, ended the job. They
// follow the conventions of GNU timeout(1) and shells.
const (
	exitNotStarted = 127
	exitTimedOut   = 124
	exitCanceled   = 130
	// exitLowMemory is for a job the agent stopped over memory (the device
	// ran low, or the job reached its limit), like one the Linux OOM killer
	// ends with SIGKILL.
	exitLowMemory = 137
)

// execute runs one job in a fresh scratch directory, streaming its output to
// fw. The process tree is killed if ctx ends (client disconnect) or the job's
// timeout passes.
func (a *Agent) execute(ctx context.Context, jobID string, req proto.RunRequest, fw *proto.FrameWriter) (int, time.Duration, error) {
	if req.TimeoutSec > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(req.TimeoutSec)*time.Second)
		defer cancel()
	}

	dir, err := os.MkdirTemp(a.cfg.WorkDir, "job-")
	if err != nil {
		return exitNotStarted, 0, fmt.Errorf("create job dir: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			a.cfg.Logger.Printf("job %s: remove %s: %v", jobID, dir, err)
		}
	}()

	program := shippedProgram(req)
	shipped, err := writeInputs(dir, req.Files, a.cache, program)
	if err != nil {
		return exitNotStarted, 0, err
	}
	if program != "" {
		argv := append([]string(nil), req.Argv...)
		argv[0] = shipped[program]
		req.Argv = argv
	}

	// stopJob ends the job with a reason: reaching its memory limit (not
	// every program fails cleanly when an allocation is refused, and some
	// stall for good) or the low-memory guard (guard.go).
	ctx, stopJob := context.WithCancelCause(ctx)
	defer stopJob(nil)
	limitMB := a.cfg.JobMemoryLimitMB

	cmd := buildCommand(ctx, req)
	cmd.Dir = dir
	cmd.Env = os.Environ()
	for k, v := range req.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	// exec dedupes Env keeping the last value, so these cannot be overridden.
	cmd.Env = append(cmd.Env, "POOL_JOB_ID="+jobID, "POOL_NODE="+a.cfg.NodeName)
	cmd.Stdout = fw.Stream(proto.FrameStdout)
	cmd.Stderr = fw.Stream(proto.FrameStderr)
	cmd.WaitDelay = 5 * time.Second
	// exec calls Cancel only if ctx ends while the process still runs, so
	// killed tells a stop that ended the job from one that came after it
	// had exited on its own.
	var killed atomic.Bool
	kill := cmd.Cancel
	cmd.Cancel = func() error {
		killed.Store(true)
		return kill()
	}

	start := time.Now()
	if err := cmd.Start(); err != nil {
		if ctx.Err() != nil { // canceled before the process could start
			if code, err := memoryStop(context.Cause(ctx)); err != nil {
				return code, 0, err
			}
			return exitCanceled, 0, errors.New("job canceled")
		}
		return exitNotStarted, 0, fmt.Errorf("start %s: %w", req.Argv[0], err)
	}
	lowerPriority(cmd)
	// capped covers the job's process and what it starts from now on, not
	// a child it started in the instant since Start (a shell's command,
	// say): that one isn't held to the limit, though stopping the job still
	// ends it (killTree), as does the low-memory guard.
	peakMemory, capped := trackMemory(cmd, limitMB, func() {
		a.cfg.Logger.Printf("job %s stopped: it reached its memory limit of %d MB", jobID, limitMB)
		stopJob(&memoryLimitError{node: a.cfg.NodeName, limitMB: limitMB})
	})
	if limitMB > 0 && JobMemoryLimitEnforced && !capped {
		a.cfg.Logger.Printf("job %s runs without its memory limit of %d MB: the limit couldn't be set with a way to stop the job at it", jobID, limitMB)
	}
	a.jobStoppable(jobID, stopJob)
	err = cmd.Wait()
	if a.afterWait != nil {
		a.afterWait()
	}
	dur := time.Since(start)
	reapTree(cmd)
	// peakMemory returns only after any limit notice has been handled, so
	// the cause below is settled.
	peak := peakMemory()

	cause := stopCause(ctx, err, killed.Load())

	if errors.Is(cause, context.DeadlineExceeded) {
		a.mem.observe(peak)
		return exitTimedOut, dur, fmt.Errorf("job timed out after %ds", req.TimeoutSec)
	}
	if code, err := memoryStop(cause); err != nil {
		// A job cut short over memory shows the limit or a partial use, not
		// what jobs need, so it doesn't teach the estimate.
		return code, dur, err
	}
	a.mem.observe(peak)
	if cause != nil {
		return exitCanceled, dur, errors.New("job canceled")
	}
	code, runErr := 0, error(nil)
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr):
		code = exitErr.ExitCode()
	case err != nil:
		code, runErr = 1, err
	}
	// Collect even after a failure: partial output often explains it.
	if len(req.Collect) > 0 {
		if cerr := collectOutputs(dir, req.Collect, fw); cerr != nil && runErr == nil {
			runErr = fmt.Errorf("collect: %w", cerr)
		}
	}
	return code, dur, runErr
}

// stopCause is why the job was stopped, read once so the checks after it
// agree, or nil if it ended on its own and keeps its own exit status. The
// guard, a timeout or a cancel can end ctx after the process has exited;
// only one that killed it (killed) counts. A job that went past its memory
// limit and then failed counts as stopped either way: the hard cap past
// the limit refuses its allocations, so that is the likely failure.
func stopCause(ctx context.Context, waitErr error, killed bool) error {
	if waitErr == nil {
		return nil
	}
	cause := context.Cause(ctx)
	var over *memoryLimitError
	if killed || errors.As(cause, &over) {
		return cause
	}
	return nil
}

// memoryStop reports the exit code and reason if the job was stopped over
// memory (cause is its context's cause), or a nil error if not.
func memoryStop(cause error) (int, error) {
	var low *lowMemoryError
	var over *memoryLimitError
	switch {
	case errors.As(cause, &low), errors.As(cause, &over):
		return exitLowMemory, cause
	}
	return 0, nil
}
