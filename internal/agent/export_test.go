package agent

// SetAfterWait has the agent call f as each job's process has exited,
// before it works out how the job ended.
func (a *Agent) SetAfterWait(f func()) { a.afterWait = f }

// RelieveMemory runs one check of the low-memory guard (GuardMemory).
func (a *Agent) RelieveMemory() bool { return a.relieveMemory() }
