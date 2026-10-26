package config

import (
	"testing"
	"time"
)

func TestVODDefaultsDisabled(t *testing.T) {
	t.Setenv("VOD_ENABLED", "")
	c, e := loadVOD()
	if e != nil || c.Enabled || c.MaxUploadBytes != 2147483648 || c.MaxDuration != time.Hour || c.ResultRetention != 7*24*time.Hour || c.OriginalRetention != 24*time.Hour || c.Concurrency != 1 || c.MinFreeMiB != 4096 {
		t.Fatalf("defaults: %+v, %v", c, e)
	}
}
func TestVODEnabledValidation(t *testing.T) {
	t.Setenv("VOD_ENABLED", "true")
	t.Setenv("VOD_WORKER_GPUS", "a:1=GPU-0,b:1=GPU-0")
	c, e := loadVOD()
	if e != nil || c.WorkerGPUs["a:1"] != "GPU-0" {
		t.Fatal(e)
	}
	for key, value := range map[string]string{"VOD_MAX_UPLOAD_BYTES": "0", "VOD_ORIGINAL_RETENTION_SEC": "86401", "VOD_RESULT_RETENTION_SEC": "0", "VOD_SWEEP_INTERVAL": "0s", "VOD_ROOT": "relative", "VOD_WORKER_GPUS": "unknown", "VOD_MAX_CONCURRENCY": "bad"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, value)
			if _, e := loadVOD(); e == nil {
				t.Fatal("invalid setting accepted")
			}
		})
	}
}
