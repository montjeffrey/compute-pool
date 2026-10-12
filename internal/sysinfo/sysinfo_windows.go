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
