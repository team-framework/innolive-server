package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// The AI worker enforces these fixed MVP limits, including on resume.
const (
	VODWorkerMaxUploadBytes int64 = 2147483648
	VODWorkerMaxDuration          = 3600 * time.Second
)

// GPU identity must identify a physical GPU across all slots on a host.
// Unknown targets fail closed for VOD; live target replacement remains unchanged.
type VODConfig struct {
	Enabled        bool
	Root           string
	MaxUploadBytes int64
	MaxDuration    time.Duration
	Concurrency    int
	MinFreeMiB     int64
	// StaticFreeMiB, when set, replaces nvidia-smi with a fixed free-memory map
	// for local/CPU E2E. Production leaves this nil and uses NvidiaMemory.
	StaticFreeMiB     *int64
	ResultRetention   time.Duration
	OriginalRetention time.Duration
	SweepInterval     time.Duration
	UploadTimeout     time.Duration
	WorkerGPUs        map[string]string
}

func DefaultVOD() VODConfig {
	return VODConfig{Root: "/var/lib/innolive/vod", MaxUploadBytes: VODWorkerMaxUploadBytes, MaxDuration: VODWorkerMaxDuration,
		Concurrency: 1, MinFreeMiB: 4096, ResultRetention: 7 * 24 * time.Hour, OriginalRetention: 24 * time.Hour,
		SweepInterval: time.Minute, UploadTimeout: 30 * time.Minute, WorkerGPUs: map[string]string{}}
}

func loadVOD() (VODConfig, error) {
	c := DefaultVOD()
	var err error
	if raw := os.Getenv("VOD_ENABLED"); raw != "" {
		c.Enabled, err = strconv.ParseBool(raw)
		if err != nil {
			return c, fmt.Errorf("invalid VOD_ENABLED")
		}
	}
	if !c.Enabled {
		return c, nil
	}
	c.Root = env("VOD_ROOT", c.Root)
	for _, v := range []struct {
		key  string
		dest *int64
	}{{"VOD_MAX_UPLOAD_BYTES", &c.MaxUploadBytes}, {"VOD_MIN_FREE_MIB", &c.MinFreeMiB}} {
		if raw := os.Getenv(v.key); raw != "" {
			n, e := strconv.ParseInt(raw, 10, 64)
			if e != nil {
				return c, fmt.Errorf("invalid %s", v.key)
			}
			*v.dest = n
		}
	}
	if raw := os.Getenv("VOD_STATIC_FREE_MIB"); raw != "" {
		n, e := strconv.ParseInt(raw, 10, 64)
		if e != nil || n < 0 {
			return c, fmt.Errorf("invalid VOD_STATIC_FREE_MIB")
		}
		c.StaticFreeMiB = &n
	}
	for _, v := range []struct {
		key  string
		dest *time.Duration
	}{{"VOD_MAX_DURATION_SEC", &c.MaxDuration}, {"VOD_RESULT_RETENTION_SEC", &c.ResultRetention}, {"VOD_ORIGINAL_RETENTION_SEC", &c.OriginalRetention}} {
		if raw := os.Getenv(v.key); raw != "" {
			n, e := strconv.ParseInt(raw, 10, 64)
			if e != nil || n < 0 || n > 315360000 {
				return c, fmt.Errorf("invalid %s", v.key)
			}
			*v.dest = time.Duration(n) * time.Second
		}
	}
	for _, v := range []struct {
		key  string
		dest *time.Duration
	}{{"VOD_SWEEP_INTERVAL", &c.SweepInterval}, {"VOD_UPLOAD_TIMEOUT", &c.UploadTimeout}} {
		if raw := os.Getenv(v.key); raw != "" {
			n, e := time.ParseDuration(raw)
			if e != nil {
				return c, fmt.Errorf("invalid %s", v.key)
			}
			*v.dest = n
		}
	}
	if raw := os.Getenv("VOD_MAX_CONCURRENCY"); raw != "" {
		c.Concurrency, err = strconv.Atoi(raw)
		if err != nil {
			return c, fmt.Errorf("invalid VOD_MAX_CONCURRENCY")
		}
	}
	for _, entry := range splitList(os.Getenv("VOD_WORKER_GPUS")) {
		target, gpu, ok := strings.Cut(entry, "=")
		if !ok || target == "" || gpu == "" {
			return c, fmt.Errorf("VOD_WORKER_GPUS requires target=GPU-UUID entries")
		}
		c.WorkerGPUs[target] = gpu
	}
	return c, c.Validate()
}

func (c VODConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	// Reject misconfiguration instead of silently advertising a different limit.
	if c.MaxUploadBytes > VODWorkerMaxUploadBytes {
		return fmt.Errorf("VOD_MAX_UPLOAD_BYTES must not exceed %d", VODWorkerMaxUploadBytes)
	}
	if c.MaxDuration > VODWorkerMaxDuration {
		return fmt.Errorf("VOD_MAX_DURATION_SEC must not exceed 3600")
	}
	if !filepath.IsAbs(c.Root) || filepath.Clean(c.Root) == "/" {
		return fmt.Errorf("VOD_ROOT must be an absolute dedicated directory")
	}
	if c.MaxUploadBytes <= 0 || c.MaxUploadBytes > 1<<60 || c.MaxDuration <= 0 || c.Concurrency < 1 || c.MinFreeMiB < 0 || c.ResultRetention <= 0 || c.OriginalRetention < 0 || c.OriginalRetention > 24*time.Hour || c.SweepInterval <= 0 || c.SweepInterval > time.Hour || c.UploadTimeout <= 0 {
		return fmt.Errorf("invalid VOD limits or retention settings")
	}
	return nil
}
