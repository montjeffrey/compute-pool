//go:build windows

package sysinfo

import (
	"sync"
	"syscall"
	"unsafe"
)

var (
	kernel32                      = syscall.NewLazyDLL("kernel32.dll")
	procGetSystemTimes            = kernel32.NewProc("GetSystemTimes")
	procGetSystemPowerStatus      = kernel32.NewProc("GetSystemPowerStatus")
	procQueryInformationJobObject = kernel32.NewProc("QueryInformationJobObject")
	procCreateJobObjectW          = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject   = kernel32.NewProc("SetInformationJobObject")
	procGetQueuedCompletionStatus = kernel32.NewProc("GetQueuedCompletionStatus")
	procAssignProcessToJobObject  = kernel32.NewProc("AssignProcessToJobObject")
	procGlobalMemoryStatusEx      = kernel32.NewProc("GlobalMemoryStatusEx")
)

// jobBasicAccounting mirrors JOBOBJECT_BASIC_ACCOUNTING_INFORMATION.
type jobBasicAccounting struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

const jobObjectBasicAccountingInformation = 1

// JobCPUTimes returns the CPU time used so far by every process that has
// been in the job object h, in GetSystemTimes' units (100 ns), so it can be
// compared with system-wide use.
func JobCPUTimes(h uintptr) (uint64, error) {
	var info jobBasicAccounting
	r, _, err := procQueryInformationJobObject.Call(h, jobObjectBasicAccountingInformation,
		uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info), 0)
	if r == 0 {
		return 0, err
	}
	return uint64(info.TotalUserTime + info.TotalKernelTime), nil
}

// Mirror JOBOBJECT_BASIC_LIMIT_INFORMATION and
// JOBOBJECT_EXTENDED_LIMIT_INFORMATION; Go's layout matches C's on Windows.
type jobBasicLimit struct {
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

type jobExtendedLimit struct {
	BasicLimitInformation jobBasicLimit
	IoInfo                [6]uint64
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

const jobObjectExtendedLimitInformation = 9

// JobPeakMemory returns the most memory, in bytes, that the processes in job
// object h have had committed at once.
func JobPeakMemory(h uintptr) (uint64, error) {
	var info jobExtendedLimit
	r, _, err := procQueryInformationJobObject.Call(h, jobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info), 0)
	if r == 0 {
		return 0, err
	}
	return uint64(info.PeakJobMemoryUsed), nil
}

const (
	jobObjectLimitJobMemory                     = 0x00000200
	jobObjectLimitKillOnJobClose                = 0x00002000
	jobObjectAssociateCompletionPortInformation = 7
	jobObjectMsgJobMemoryLimit                  = 10
)

// jobAssociateCompletionPort mirrors JOBOBJECT_ASSOCIATE_COMPLETION_PORT.
type jobAssociateCompletionPort struct {
	CompletionKey  uintptr
	CompletionPort uintptr
}

// TrackMemory puts process pid, and the processes it starts from now on, in
// a job object of their own so their combined peak memory can be read. The
// returned function reports that peak in bytes (0 if unknown) and releases
// the job object, which ends any process still in it; call it once, after
// the process has exited. A process that is already in the agent's job
// object nests this one inside it.
//
// If limitBytes is above zero, the processes together can't commit more
// than that: an allocation beyond it fails, as it would on a device out of
// memory, instead of pushing the owner's programs out to the page file.
// Programs don't all fail cleanly when that happens (a Go program can stall
// for good), so onLimit, if not nil, is called once when the job first
// tries to go past the limit, for the caller to end it.
func TrackMemory(pid int, limitBytes uint64, onLimit func()) func() uint64 {
	none := func() uint64 { return 0 }
	job, _, _ := procCreateJobObjectW.Call(0, 0)
	if job == 0 {
		return none
	}
	// Closing the job ends what the program left running, as killing the
	// process group does on Unix; without the limit the job is still
	// measured, so a failure here doesn't stop it from running.
	var info jobExtendedLimit
	info.BasicLimitInformation.LimitFlags = jobObjectLimitKillOnJobClose
	if limitBytes > 0 {
		info.BasicLimitInformation.LimitFlags |= jobObjectLimitJobMemory
		info.JobMemoryLimit = uintptr(limitBytes)
	}
	r, _, _ := procSetInformationJobObject.Call(job, jobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info))
	var w *limitWatch
	if r != 0 && limitBytes > 0 && onLimit != nil {
		w = watchMemoryLimit(job, onLimit)
	}
	release := func() {
		if w != nil {
			w.stop()
		}
		syscall.CloseHandle(syscall.Handle(job))
	}
	const processSetQuota, processTerminate = 0x0100, 0x0001
	h, err := syscall.OpenProcess(processSetQuota|processTerminate, false, uint32(pid))
	if err != nil {
		release()
		return none
	}
	r, _, _ = procAssignProcessToJobObject.Call(job, uintptr(h))
	syscall.CloseHandle(h)
	if r == 0 {
		release()
		return none
	}
	return func() uint64 {
		defer release()
		peak, _ := JobPeakMemory(job)
		return peak
	}
}

// quitWatching, posted with key 0, tells a limitWatch's goroutine to stop;
// the job's own messages carry the job handle as their key.
const quitWatching = 0xFFFFFFFF

// limitWatch waits for a job's memory-limit notice on a completion port.
type limitWatch struct {
	port uintptr
	done chan struct{}
}

// watchMemoryLimit has job report to a new completion port and calls
// onLimit once when the job's memory limit is hit. It returns nil if the
// port can't be set up.
func watchMemoryLimit(job uintptr, onLimit func()) *limitWatch {
	p, err := syscall.CreateIoCompletionPort(syscall.InvalidHandle, 0, 0, 1)
	if err != nil {
		return nil
	}
	w := &limitWatch{port: uintptr(p), done: make(chan struct{})}
	assoc := jobAssociateCompletionPort{CompletionKey: job, CompletionPort: w.port}
	r, _, _ := procSetInformationJobObject.Call(job, jobObjectAssociateCompletionPortInformation,
		uintptr(unsafe.Pointer(&assoc)), unsafe.Sizeof(assoc))
	if r == 0 {
		syscall.CloseHandle(p)
		return nil
	}
	go func() {
		defer close(w.done)
		var once sync.Once
		for {
			var msg uint32
			var key, overlapped uintptr
			// The message comes in the byte count. The call fails only if
			// the port was closed (see stop).
			r, _, _ := procGetQueuedCompletionStatus.Call(w.port, uintptr(unsafe.Pointer(&msg)),
				uintptr(unsafe.Pointer(&key)), uintptr(unsafe.Pointer(&overlapped)), syscall.INFINITE)
			switch {
			case r == 0:
				return
			case key == 0 && msg == quitWatching:
				// Closed here, not in stop, so the handle can't be reused
				// while this goroutine still waits on it.
				syscall.CloseHandle(syscall.Handle(w.port))
				return
			case key == job && msg == jobObjectMsgJobMemoryLimit:
				once.Do(onLimit)
			}
		}
	}()
	return w
}

// stop ends the watching and returns once every notice already queued,
// including a memory-limit one, has been handled.
func (w *limitWatch) stop() {
	if err := syscall.PostQueuedCompletionStatus(syscall.Handle(w.port), quitWatching, 0, nil); err != nil {
		// Closing the port wakes the goroutine instead; it then returns
		// without closing it again.
		syscall.CloseHandle(syscall.Handle(w.port))
	}
	<-w.done
}

// memoryStatusEx mirrors MEMORYSTATUSEX.
type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

func readMemory() (Memory, error) {
	m := memoryStatusEx{Length: uint32(unsafe.Sizeof(memoryStatusEx{}))}
	if r, _, err := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m))); r == 0 {
		return Memory{}, err
	}
	return Memory{Known: true, TotalMB: int64(m.TotalPhys >> 20), FreeMB: int64(m.AvailPhys >> 20)}, nil
}

func filetime(ft syscall.Filetime) uint64 {
	return uint64(ft.HighDateTime)<<32 | uint64(ft.LowDateTime)
}

func readCPUTimes() (cpuTimes, error) {
	var idle, kernel, user syscall.Filetime
	r, _, err := procGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&idle)),
		uintptr(unsafe.Pointer(&kernel)),
		uintptr(unsafe.Pointer(&user)),
	)
	if r == 0 {
		return cpuTimes{}, err
	}
	// Kernel time already includes idle time.
	return cpuTimes{idle: filetime(idle), total: filetime(kernel) + filetime(user)}, nil
}

// systemPowerStatus mirrors SYSTEM_POWER_STATUS.
type systemPowerStatus struct {
	ACLineStatus        byte
	BatteryFlag         byte
	BatteryLifePercent  byte
	SystemStatusFlag    byte
	BatteryLifeTime     uint32
	BatteryFullLifeTime uint32
}

const (
	batteryFlagNoBattery = 128
	batteryFlagUnknown   = 255
)

func readPower() (Power, error) {
	var s systemPowerStatus
	r, _, err := procGetSystemPowerStatus.Call(uintptr(unsafe.Pointer(&s)))
	if r == 0 {
		return Power{}, err
	}
	p := Power{Known: true, BatteryPct: -1}
	p.HasBattery = s.BatteryFlag&batteryFlagNoBattery == 0 && s.BatteryFlag != batteryFlagUnknown
	p.OnBattery = p.HasBattery && s.ACLineStatus == 0
	if p.HasBattery && s.BatteryLifePercent <= 100 {
		p.BatteryPct = int(s.BatteryLifePercent)
	}
	return p, nil
}
