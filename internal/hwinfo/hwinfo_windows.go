package hwinfo

import (
	"errors"
	"syscall"
	"unsafe"
)

var (
	kernel32                             = syscall.NewLazyDLL("kernel32.dll")
	procGetLogicalProcessorInformationEx = kernel32.NewProc("GetLogicalProcessorInformationEx")
	procGlobalMemoryStatusEx             = kernel32.NewProc("GlobalMemoryStatusEx")
)

func detect() Specs {
	s := Specs{CPUModel: cpuModel(), MemoryMB: memoryMB()}
	if classes, err := coreClasses(); err == nil {
		s.PhysicalCores = len(classes)
		s.PerfCores, s.EffCores = splitClasses(classes)
	}
	return s
}

// cpuModel reads the processor's marketing name from the registry.
func cpuModel() string {
	path, _ := syscall.UTF16PtrFromString(`HARDWARE\DESCRIPTION\System\CentralProcessor\0`)
	var key syscall.Handle
	if syscall.RegOpenKeyEx(syscall.HKEY_LOCAL_MACHINE, path, 0, syscall.KEY_READ, &key) != nil {
		return ""
	}
	defer syscall.RegCloseKey(key)
	name, _ := syscall.UTF16PtrFromString("ProcessorNameString")
	buf := make([]uint16, 256)
	n := uint32(len(buf) * 2)
	var typ uint32
	if syscall.RegQueryValueEx(key, name, nil, &typ, (*byte)(unsafe.Pointer(&buf[0])), &n) != nil || typ != syscall.REG_SZ {
		return ""
	}
	return syscall.UTF16ToString(buf[:n/2])
}

// coreClasses returns one EfficiencyClass per physical core.
func coreClasses() ([]int, error) {
	var n uint32
	// The first call reports the buffer size needed.
	procGetLogicalProcessorInformationEx.Call(relationProcessorCore, 0, uintptr(unsafe.Pointer(&n)))
	if n == 0 {
		return nil, errors.New("GetLogicalProcessorInformationEx returned no size")
	}
	buf := make([]byte, n)
	r, _, err := procGetLogicalProcessorInformationEx.Call(relationProcessorCore,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n)))
	if r == 0 {
		return nil, err
	}
	return parseProcessorCores(buf[:n], int(unsafe.Sizeof(uintptr(0))))
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

func memoryMB() int64 {
	m := memoryStatusEx{Length: uint32(unsafe.Sizeof(memoryStatusEx{}))}
	if r, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m))); r == 0 {
		return 0
	}
	return int64(m.TotalPhys / (1 << 20))
}
