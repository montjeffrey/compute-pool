package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
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

	start := time.Now()
	if err := cmd.Start(); err != nil {
		if ctx.Err() != nil { // canceled before the process could start
			if code, err := memoryStop(ctx); err != nil {
				return code, 0, err
			}
			return exitCanceled, 0, errors.New("job canceled")
		}
		return exitNotStarted, 0, fmt.Errorf("start %s: %w", req.Argv[0], err)
	}
	lowerPriority(cmd)
	peakMemory := trackMemory(cmd, limitMB, func() {
		a.cfg.Logger.Printf("job %s stopped: it reached its memory limit of %d MB", jobID, limitMB)
		stopJob(&memoryLimitError{node: a.cfg.NodeName, limitMB: limitMB})
	})
	a.jobStoppable(jobID, stopJob)
	err = cmd.Wait()
	dur := time.Since(start)
	reapTree(cmd)
	// peakMemory returns only after any limit notice has been handled, so
	// the cause below is settled.
	peak := peakMemory()

	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		a.mem.observe(peak)
		return exitTimedOut, dur, fmt.Errorf("job timed out after %ds", req.TimeoutSec)
	}
	if code, err := memoryStop(ctx); err != nil {
		// A job cut short over memory shows the limit or a partial use, not
		// what jobs need, so it doesn't teach the estimate.
		return code, dur, err
	}
	a.mem.observe(peak)
	if ctx.Err() != nil {
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

// memoryStop reports the exit code and reason if ctx was ended over memory,
// or a nil error if not.
func memoryStop(ctx context.Context) (int, error) {
	var low *lowMemoryError
	var over *memoryLimitError
	switch cause := context.Cause(ctx); {
	case errors.As(cause, &low), errors.As(cause, &over):
		return exitLowMemory, cause
	}
	return 0, nil
}
