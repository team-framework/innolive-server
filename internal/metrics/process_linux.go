//go:build linux

package metrics

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

func readProcessStats() (float64, uint64) {
	var usage syscall.Rusage
	cpuSeconds := 0.0
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err == nil {
		cpuSeconds = timevalSeconds(usage.Utime) + timevalSeconds(usage.Stime)
	}

	contents, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return cpuSeconds, 0
	}
	fields := strings.Fields(string(contents))
	if len(fields) < 2 {
		return cpuSeconds, 0
	}
	residentPages, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return cpuSeconds, 0
	}
	return cpuSeconds, residentPages * uint64(os.Getpagesize())
}

func timevalSeconds(value syscall.Timeval) float64 {
	return float64(value.Sec) + float64(value.Usec)/1e6
}

func readChildProcessStats(pid int) (float64, uint64, bool) {
	schedulerContents, err := os.ReadFile(fmt.Sprintf("/proc/%d/schedstat", pid))
	if err != nil {
		return 0, 0, false
	}
	schedulerFields := strings.Fields(string(schedulerContents))
	if len(schedulerFields) == 0 {
		return 0, 0, false
	}
	runtimeNanoseconds, err := strconv.ParseUint(schedulerFields[0], 10, 64)
	if err != nil {
		return 0, 0, false
	}

	residentBytes := uint64(0)
	memoryContents, err := os.ReadFile(fmt.Sprintf("/proc/%d/statm", pid))
	if err == nil {
		memoryFields := strings.Fields(string(memoryContents))
		if len(memoryFields) >= 2 {
			if residentPages, parseErr := strconv.ParseUint(memoryFields[1], 10, 64); parseErr == nil {
				residentBytes = residentPages * uint64(os.Getpagesize())
			}
		}
	}
	return float64(runtimeNanoseconds) / 1e9, residentBytes, true
}
