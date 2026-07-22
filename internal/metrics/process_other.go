//go:build !linux && !windows

package metrics

func readProcessStats() (float64, uint64) {
	return 0, 0
}

func readChildProcessStats(int) (float64, uint64, bool) {
	return 0, 0, false
}
