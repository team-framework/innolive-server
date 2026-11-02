package metrics

import (
	"strconv"
	"strings"
)

// procClockTicksPerSecond는 /proc/<pid>/stat의 시간 단위(USER_HZ)다. 리눅스 사용자
// 공간에 노출되는 값은 아키텍처와 관계없이 100이다.
const procClockTicksPerSecond = 100

// procStatCPUSeconds는 /proc/<pid>/stat 내용에서 프로세스의 모든 스레드가 쓴 CPU
// 시간(utime+stime)을 초로 돌려준다. schedstat은 메인 스레드 하나만 세서
// 멀티스레드 인코더 사용량을 크게 줄여 보인다(P1-32).
func procStatCPUSeconds(contents string) (float64, bool) {
	// 둘째 필드(comm)는 괄호 안에 공백을 가질 수 있으므로 마지막 ')' 뒤부터 센다.
	end := strings.LastIndexByte(contents, ')')
	if end < 0 {
		return 0, false
	}
	// ')' 뒤 첫 필드가 3번(state)이다. utime은 14번, stime은 15번이다.
	fields := strings.Fields(contents[end+1:])
	if len(fields) < 13 {
		return 0, false
	}
	utime, err := strconv.ParseUint(fields[11], 10, 64)
	if err != nil {
		return 0, false
	}
	stime, err := strconv.ParseUint(fields[12], 10, 64)
	if err != nil {
		return 0, false
	}
	return float64(utime+stime) / procClockTicksPerSecond, true
}
