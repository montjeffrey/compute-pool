//go:build windows

package main

import (
	"fmt"
	"os/exec"
	"syscall"
	"unsafe"
)

var (
	kernel32                     = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW         = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject  = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObject = kernel32.NewProc("AssignProcessToJobObject")
	procFreeConsole              = kernel32.NewProc("FreeConsole")
)

const (
	jobObjectExtendedLimitInformation = 9
	jobObjectLimitKillOnJobClose      = 0x2000
)

// Mirrors JOBOBJECT_BASIC_LIMIT_INFORMATION and
// JOBOBJECT_EXTENDED_LIMIT_INFORMATION; Go's layout matches C's on Windows.
type jobBasicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type ioCounters struct {
	ReadOperationCount, WriteOperationCount, OtherOperationCount uint64
	ReadTransferCount, WriteTransferCount, OtherTransferCount    uint64
}

type jobExtendedLimitInformation struct {
	BasicLimitInformation jobBasicLimitInformation
	IoInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

// containChildren places the agent in a job object that kills every process
// in it when the last handle closes, which happens when the agent exits for
// any reason. Jobs inherit the job object, so none can outlive a stopped or
// crashed agent.
func containChildren() error {
	h, _, err := procCreateJobObjectW.Call(0, 0)
	if h == 0 {
		return fmt.Errorf("create job object: %w", err)
	}
	var info jobExtendedLimitInformation
	info.BasicLimitInformation.LimitFlags = jobObjectLimitKillOnJobClose
	r, _, err := procSetInformationJobObject.Call(h, jobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info))
	if r == 0 {
		syscall.CloseHandle(syscall.Handle(h))
		return fmt.Errorf("configure job object: %w", err)
	}
	self, _ := syscall.GetCurrentProcess()
	if r, _, err := procAssignProcessToJobObject.Call(h, uintptr(self)); r == 0 {
		syscall.CloseHandle(syscall.Handle(h))
		return fmt.Errorf("join job object: %w", err)
	}
	// The handle is deliberately never closed: the OS closes it when the
	// agent exits, and that is what ends the jobs.
	return nil
}

// hideConsole detaches from the console window the startup task creates.
func hideConsole() { procFreeConsole.Call() }

// tokenElevation is TOKEN_INFORMATION_CLASS's TokenElevation.
const tokenElevation = 20

// isElevated reports whether this process runs as administrator, which
// pool install needs to start the agent at boot rather than at logon.
func isElevated() bool {
	p, err := syscall.GetCurrentProcess()
	if err != nil {
		return false
	}
	var t syscall.Token
	if syscall.OpenProcessToken(p, syscall.TOKEN_QUERY, &t) != nil {
		return false
	}
	defer t.Close()
	var elevated, n uint32
	if syscall.GetTokenInformation(t, tokenElevation, (*byte)(unsafe.Pointer(&elevated)), 4, &n) != nil {
		return false
	}
	return elevated != 0
}

func openBrowser(url string) error {
	return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}
