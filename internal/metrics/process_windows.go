//go:build windows

package metrics

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var getProcessMemoryInfo = windows.NewLazySystemDLL("psapi.dll").NewProc("GetProcessMemoryInfo")

type processMemoryCounters struct {
	Size                       uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

func readProcessStats() (float64, uint64) {
	process, err := windows.GetCurrentProcess()
	if err != nil {
		return 0, 0
	}
	cpuSeconds, rssBytes, _ := readWindowsProcessStats(process)
	return cpuSeconds, rssBytes
}

func readChildProcessStats(pid int) (float64, uint64, bool) {
	process, err := windows.OpenProcess(
		windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ,
		false,
		uint32(pid),
	)
	if err != nil {
		process, err = windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
		if err != nil {
			return 0, 0, false
		}
	}
	defer windows.CloseHandle(process)
	return readWindowsProcessStats(process)
}

func readWindowsProcessStats(process windows.Handle) (float64, uint64, bool) {
	var creationTime windows.Filetime
	var exitTime windows.Filetime
	var kernelTime windows.Filetime
	var userTime windows.Filetime
	cpuSeconds := 0.0
	ok := false
	if err := windows.GetProcessTimes(process, &creationTime, &exitTime, &kernelTime, &userTime); err == nil {
		cpuSeconds = filetimeSeconds(kernelTime) + filetimeSeconds(userTime)
		ok = true
	}

	counters := processMemoryCounters{Size: uint32(unsafe.Sizeof(processMemoryCounters{}))}
	result, _, _ := getProcessMemoryInfo.Call(
		uintptr(process),
		uintptr(unsafe.Pointer(&counters)),
		uintptr(counters.Size),
	)
	if result != 0 {
		ok = true
		return cpuSeconds, uint64(counters.WorkingSetSize), ok
	}
	return cpuSeconds, 0, ok
}

func filetimeSeconds(value windows.Filetime) float64 {
	ticks := uint64(value.HighDateTime)<<32 | uint64(value.LowDateTime)
	return float64(ticks) / 1e7
}
