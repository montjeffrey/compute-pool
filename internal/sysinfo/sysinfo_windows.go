//go:build windows

package sysinfo

import (
	"syscall"
	"unsafe"
)

var (
	kernel32                 = syscall.NewLazyDLL("kernel32.dll")
	procGetSystemTimes       = kernel32.NewProc("GetSystemTimes")
	procGetSystemPowerStatus = kernel32.NewProc("GetSystemPowerStatus")
)

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
