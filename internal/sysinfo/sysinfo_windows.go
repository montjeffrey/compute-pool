//go:build windows

package sysinfo

import (
	"syscall"
	"unsafe"
)

var (
	kernel32                      = syscall.NewLazyDLL("kernel32.dll")
	procGetSystemTimes            = kernel32.NewProc("GetSystemTimes")
	procGetSystemPowerStatus      = kernel32.NewProc("GetSystemPowerStatus")
	procQueryInformationJobObject = kernel32.NewProc("QueryInformationJobObject")
	procCreateJobObjectW          = kernel32.NewProc("CreateJobObjectW")
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

// TrackMemory puts process pid, and the processes it starts from now on, in
// a job object of their own so their combined peak memory can be read. The
// returned function reports that peak in bytes (0 if unknown) and releases
// the job object; call it once, after the process has exited. A process that
// is already in the agent's job object nests this one inside it.
func TrackMemory(pid int) func() uint64 {
	none := func() uint64 { return 0 }
	job, _, _ := procCreateJobObjectW.Call(0, 0)
	if job == 0 {
		return none
	}
	const processSetQuota, processTerminate = 0x0100, 0x0001
	h, err := syscall.OpenProcess(processSetQuota|processTerminate, false, uint32(pid))
	if err != nil {
		syscall.CloseHandle(syscall.Handle(job))
		return none
	}
	r, _, _ := procAssignProcessToJobObject.Call(job, uintptr(h))
	syscall.CloseHandle(h)
	if r == 0 {
		syscall.CloseHandle(syscall.Handle(job))
		return none
	}
	return func() uint64 {
		defer syscall.CloseHandle(syscall.Handle(job))
		peak, _ := JobPeakMemory(job)
		return peak
	}
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
