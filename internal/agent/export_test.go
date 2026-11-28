package agent

// SetAfterWait has the agent call f as each job's process has exited,
// before it works out how the job ended.
func (a *Agent) SetAfterWait(f func()) { a.afterWait = f }

// SetBeforeWait has the agent call f with each job's process ID as the
// process has started, before it waits for the process.
func (a *Agent) SetBeforeWait(f func(pid int)) { a.beforeWait = f }

// SetOnCancel has the agent call f each time exec stops a job's process,
// with the result of the kill.
func (a *Agent) SetOnCancel(f func(killErr error)) { a.onCancel = f }

// RelieveMemory runs one check of the low-memory guard (GuardMemory).
func (a *Agent) RelieveMemory() bool { return a.relieveMemory() }
