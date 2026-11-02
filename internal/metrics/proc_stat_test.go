package metrics

import "testing"

func TestProcStatCPUSecondsSumsAllThreads(t *testing.T) {
	// comm에 공백과 괄호가 있어도 마지막 ')' 뒤부터 센다. utime 1234, stime 566 tick.
	contents := "4242 (ffmpeg (enc) x) S 1 4242 4242 0 -1 4194560 100 0 0 0 1234 566 0 0 20 0 9 0 12345 1000 200 18446744073709551615"
	got, ok := procStatCPUSeconds(contents)
	if !ok || got != 18.0 {
		t.Fatalf("procStatCPUSeconds = %v, %v; want 18s", got, ok)
	}
	if _, ok := procStatCPUSeconds("4242 (ffmpeg) S 1"); ok {
		t.Fatal("truncated stat was accepted")
	}
}
