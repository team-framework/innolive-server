package vod

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"inno-live-server/internal/ai"
)

// TestRealAIUploadProcessDownload exercises upload → scheduler → VodProcessor → result
// against a live worker. Opt in with:
//
//	TEST_DATABASE_URL=postgres://... \
//	VOD_E2E_AI_ADDR=127.0.0.1:50051 \
//	VOD_E2E_ROOT=/shared/vod-root \
//	VOD_E2E_SAMPLE=/path/to/sample.mp4 \
//	go test -count=1 -timeout 10m ./internal/vod -run TestRealAIUploadProcessDownload
//
// VOD_E2E_ROOT must match the worker --vod-root. Uses StaticMemory (no nvidia-smi).
func TestRealAIUploadProcessDownload(t *testing.T) {
	addr := os.Getenv("VOD_E2E_AI_ADDR")
	root := os.Getenv("VOD_E2E_ROOT")
	sample := os.Getenv("VOD_E2E_SAMPLE")
	if addr == "" || root == "" || sample == "" {
		t.Skip("VOD_E2E_AI_ADDR, VOD_E2E_ROOT, and VOD_E2E_SAMPLE required for real AI E2E")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe required")
	}
	if _, err := os.Stat(sample); err != nil {
		t.Fatalf("sample: %v", err)
	}
	if err := os.MkdirAll(root, 0o770); err != nil {
		t.Fatal(err)
	}

	s := testService(t)
	s.Config.Root = root
	s.Config.WorkerGPUs = map[string]string{addr: "CPU-E2E"}
	s.Config.MinFreeMiB = 0
	s.Probe = FFProbe{}

	pool, err := ai.NewPool([]string{addr}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })

	engine := NewEngine(s, pool, GRPCWorker{Pool: pool}, StaticMemory{Free: 8192, GPUs: s.Config.WorkerGPUs})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); engine.wg.Wait() })
	go engine.Run(ctx)

	user := uuid.New()
	body, contentType := multipartFile(t, sample, "e2e-sample.mp4")
	w := request(t, handler(s), http.MethodPost, "/vod/jobs", user, body, contentType)
	if w.Code != http.StatusCreated {
		t.Fatalf("upload %d: %s", w.Code, w.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	jobID, _ := created["job_id"].(string)
	if jobID == "" {
		t.Fatalf("missing job_id: %v", created)
	}
	if name, _ := created["original_filename"].(string); name != "e2e-sample.mp4" {
		t.Fatalf("original_filename = %v", created["original_filename"])
	}

	deadline := time.Now().Add(8 * time.Minute)
	var status string
	for time.Now().Before(deadline) {
		gw := request(t, handler(s), http.MethodGet, "/vod/jobs/"+jobID, user, nil, "")
		if gw.Code != http.StatusOK {
			t.Fatalf("get %d: %s", gw.Code, gw.Body.String())
		}
		var job map[string]any
		if err := json.Unmarshal(gw.Body.Bytes(), &job); err != nil {
			t.Fatal(err)
		}
		status, _ = job["status"].(string)
		if status == "done" || status == "failed" {
			if status == "failed" {
				t.Fatalf("job failed: %v", job["failure"])
			}
			break
		}
		time.Sleep(2 * time.Second)
	}
	if status != "done" {
		t.Fatalf("timed out status=%s", status)
	}

	rw := request(t, handler(s), http.MethodGet, "/vod/jobs/"+jobID+"/result", user, nil, "")
	if rw.Code != http.StatusOK {
		t.Fatalf("result %d: %s", rw.Code, rw.Body.String())
	}
	out := filepath.Join(t.TempDir(), "result.mp4")
	if err := os.WriteFile(out, rw.Body.Bytes(), 0o640); err != nil {
		t.Fatal(err)
	}
	if len(rw.Body.Bytes()) < 1024 {
		t.Fatalf("result too small: %d", len(rw.Body.Bytes()))
	}
	cmd := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=codec_type,codec_name", "-of", "csv=p=0", out)
	probe, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ffprobe: %v %s", err, probe)
	}
	if !bytes.Contains(probe, []byte("h264")) || !bytes.Contains(probe, []byte("aac")) {
		t.Fatalf("unexpected codecs: %s", probe)
	}
}

func multipartFile(t *testing.T, path, filename string) (*bytes.Buffer, string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b := new(bytes.Buffer)
	m := multipart.NewWriter(b)
	headers := textproto.MIMEHeader{}
	headers.Set("Content-Disposition", `form-data; name="file"; filename="`+filename+`"`)
	headers.Set("Content-Type", "video/mp4")
	part, err := m.CreatePart(headers)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(part, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	return b, m.FormDataContentType()
}
