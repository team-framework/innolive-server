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

func TestVODStaticFreeMemory(t *testing.T) {
	t.Setenv("VOD_ENABLED", "true")
	t.Setenv("VOD_WORKER_GPUS", "127.0.0.1:50051=CPU-LOCAL")
	t.Setenv("VOD_STATIC_FREE_MIB", "8192")
	c, err := loadVOD()
	if err != nil || c.StaticFreeMiB == nil || *c.StaticFreeMiB != 8192 {
		t.Fatalf("static free: %+v %v", c.StaticFreeMiB, err)
	}
	t.Setenv("VOD_STATIC_FREE_MIB", "-1")
	if _, err := loadVOD(); err == nil {
		t.Fatal("negative static free accepted")
	}
}

func TestVODWorkerLimitValidation(t *testing.T) {
	for _, tc := range []struct {
		name, bytes, seconds string
		valid                bool
	}{
		{"defaults", "", "", true},
		{"exact", "2147483648", "3600", true},
		{"lower", "2147483647", "3599", true},
		{"bytes_plus_one", "2147483649", "3600", false},
		{"seconds_plus_one", "2147483648", "3601", false},
		{"zero_duration", "2147483648", "0", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("VOD_ENABLED", "true")
			t.Setenv("VOD_MAX_UPLOAD_BYTES", tc.bytes)
			t.Setenv("VOD_MAX_DURATION_SEC", tc.seconds)
			if _, err := loadVOD(); (err == nil) != tc.valid {
				t.Fatalf("valid=%v, error=%v", tc.valid, err)
			}
		})
	}
}

func TestVODUploadLimits(t *testing.T) {
	t.Setenv("VOD_ENABLED", "true")
	t.Setenv("VOD_WORKER_GPUS", "a:1=GPU-0")
	c, err := loadVOD()
	if err != nil || c.MaxActiveJobsPerUser != 3 || c.MaxUploadsPerUser != 1 || c.MaxConcurrentUploads != 4 || c.DiskReserveMiB != 10240 {
		t.Fatalf("upload limit defaults: %+v %v", c, err)
	}
	t.Setenv("VOD_MAX_ACTIVE_JOBS_PER_USER", "5")
	t.Setenv("VOD_MAX_UPLOADS_PER_USER", "2")
	t.Setenv("VOD_MAX_CONCURRENT_UPLOADS", "8")
	t.Setenv("VOD_DISK_RESERVE_MIB", "0")
	if c, err = loadVOD(); err != nil || c.MaxActiveJobsPerUser != 5 || c.MaxUploadsPerUser != 2 || c.MaxConcurrentUploads != 8 || c.DiskReserveMiB != 0 {
		t.Fatalf("upload limit overrides: %+v %v", c, err)
	}
	for key, value := range map[string]string{"VOD_MAX_ACTIVE_JOBS_PER_USER": "0", "VOD_MAX_UPLOADS_PER_USER": "bad", "VOD_MAX_CONCURRENT_UPLOADS": "0", "VOD_DISK_RESERVE_MIB": "-1"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, value)
			if _, err := loadVOD(); err == nil {
				t.Fatal("invalid upload limit accepted")
			}
		})
	}
}
