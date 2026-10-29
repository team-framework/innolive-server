package vod

import (
	"context"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	pb "inno-live-server/api/gen/aiv1"
	"inno-live-server/internal/ai"
)

// Explicit expectations cover every interface-A §6 row, plus transport errors
// and UNIMPLEMENTED from workers that do not yet expose the VOD service.
func TestWorkerFailureTable(t *testing.T) {
	tests := []struct {
		key, state, code string
		http             int
		reconcile        bool
	}{
		{"rpc:InvalidArgument", "failed", "bad_request", 400, false},
		{"rpc:FailedPrecondition", "failed", "processing_unsupported", 422, false},
		{"rpc:Unimplemented", "failed", "processing_unsupported", 422, false},
		{"rpc:NotFound", "paused", "", 409, false},
		{"rpc:AlreadyExists", "processing", "", 409, true},
		{"rpc:ResourceExhausted", "paused", "", 409, false},
		{"rpc:Unavailable", "processing", "", 409, true},
		{"rpc:DeadlineExceeded", "processing", "", 409, true},
		{"rpc:Canceled", "processing", "", 409, true},
		{"job:INPUT_NOT_FOUND", "failed", "storage_failed", 503, false},
		{"job:INPUT_UNREADABLE", "failed", "invalid_media", 422, false},
		{"job:INPUT_UNSUPPORTED", "failed", "unsupported_media_type", 415, false},
		{"job:INPUT_TOO_LARGE", "failed", "upload_too_large", 413, false},
		{"job:INPUT_TOO_LONG", "failed", "duration_exceeded", 422, false},
		{"job:PATH_OUTSIDE_ROOT", "failed", "processing_failed", 500, false},
		{"job:OUTPUT_EXISTS", "failed", "processing_failed", 500, false},
		{"job:CHECKPOINT_INVALID", "failed", "processing_failed", 500, false},
		{"job:CHECKPOINT_MISMATCH", "failed", "processing_failed", 500, false},
		{"job:EMBEDDING_MODEL_MISMATCH", "failed", "processing_failed", 500, false},
		{"job:WHITELIST_SESSION_NOT_FOUND", "failed", "processing_failed", 500, false},
		{"job:DECODER_FAILED", "failed", "invalid_media", 422, false},
		{"job:ENCODER_FAILED", "failed", "processing_failed", 500, false},
		{"job:MUX_FAILED", "failed", "processing_failed", 500, false},
		{"job:DISK_FULL", "failed", "storage_failed", 503, false},
		{"job:RUNTIME_FAILED", "paused", "", 409, false},
		{"job:CANCELLED", "cancelled", "job_cancelled", 409, false},
		{"job:INTERNAL", "failed", "processing_failed", 500, false},
	}
	if len(tests) != len(workerFailures) {
		t.Fatal("mapping coverage changed")
	}
	for _, tc := range tests {
		t.Run(tc.key, func(t *testing.T) {
			got := mapWorkerFailure(tc.key)
			if got != (workerFailure{tc.state, tc.code, tc.http, tc.reconcile}) {
				t.Fatalf("mapping=%+v", got)
			}
			if strings.HasPrefix(tc.key, "rpc:") {
				for code := codes.Canceled; code <= codes.Unauthenticated; code++ {
					if "rpc:"+code.String() == tc.key && mapRPCFailure(status.Error(code, "private worker path")) != got {
						t.Fatal("RPC adapter differs")
					}
				}
				return
			}
			s := testService(t)
			e := NewEngine(s, nil, nil, nil)
			j := createJob(t, s, "processing")
			j.WorkerTarget = "private-worker"
			saveJob(t, s, &j)
			v := &pb.VodJobStatus{JobId: j.ID.String(), State: pb.VodJobState_VOD_JOB_STATE_FAILED, ErrorCode: strings.TrimPrefix(tc.key, "job:"), ErrorMessage: "private worker path", CheckpointToken: "checkpoint"}
			if err := e.apply(context.Background(), &j, v); err != nil {
				t.Fatal(err)
			}
			persisted, err := s.store.Load().Get(context.Background(), j.ID, j.UserID)
			if err != nil || persisted.Status != tc.state || persisted.FailureCode != tc.code {
				t.Fatalf("job=%+v err=%v", persisted, err)
			}
			if tc.state == "paused" && (persisted.CompletedAt != nil || persisted.OriginalDeleteAt != nil || persisted.CheckpointToken != "checkpoint" || persisted.WorkerTarget != "private-worker") {
				t.Fatal("retry lost checkpoint/target or started retention")
			}
			h := handler(s)
			w := request(t, h, "GET", "/vod/jobs/"+j.ID.String(), j.UserID, nil, "")
			if w.Code != 200 || strings.Contains(w.Body.String(), "private") || (tc.code != "" && !strings.Contains(w.Body.String(), `"code":"`+tc.code+`"`)) {
				t.Fatal(w.Code, w.Body.String())
			}
			w = request(t, h, "GET", "/vod/jobs/"+j.ID.String()+"/result", j.UserID, nil, "")
			wantCode := tc.code
			if wantCode == "" {
				wantCode = "result_not_ready"
			}
			assertPublicError(t, w, tc.http, wantCode)
		})
	}
}

func assertPublicError(t *testing.T, w *httptest.ResponseRecorder, http int, code string) {
	t.Helper()
	if w.Code != http || !strings.Contains(w.Body.String(), `"code":"`+code+`"`) || strings.Contains(w.Body.String(), "private") {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestUnknownWorkerErrorsAreSanitized(t *testing.T) {
	for _, key := range []string{"job:NEW_CODE/private", "job:", "rpc:Unknown", "rpc:Internal"} {
		t.Run(key, func(t *testing.T) {
			m := mapWorkerFailure(key)
			if m.State != "failed" || m.Code != "processing_failed" || m.HTTP != 500 {
				t.Fatal(m)
			}
		})
	}
	if m := mapRPCFailure(status.Error(codes.Unknown, "private")); !m.Reconcile || m.Code != "processing_failed" {
		t.Fatal(m)
	}
	s := testService(t)
	j := createJob(t, s, "failed")
	j.FailureCode = "private old unknown code"
	saveJob(t, s, &j)
	w := request(t, handler(s), "GET", "/vod/jobs/"+j.ID.String(), j.UserID, nil, "")
	if w.Code != 200 || strings.Contains(w.Body.String(), "private") || !strings.Contains(w.Body.String(), "processing_failed") {
		t.Fatal(w.Body.String())
	}
	assertPublicError(t, request(t, handler(s), "GET", "/vod/jobs/"+j.ID.String()+"/result", j.UserID, nil, ""), 500, "processing_failed")
}

type idleGRPCWorker struct{ GRPCWorker }

func (idleGRPCWorker) LiveStreams(context.Context, string) (uint64, error) { return 0, nil }

func TestGRPCWorkerFailureAPIAndCapacityRetry(t *testing.T) {
	for _, tc := range []string{"INPUT_TOO_LARGE", "RESOURCE_EXHAUSTED"} {
		t.Run(tc, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			server := grpc.NewServer()
			fake := &rpcWorker{}
			if tc == "INPUT_TOO_LARGE" {
				fake.startFailure = tc
			} else {
				fake.startError = status.Error(codes.ResourceExhausted, "private worker capacity")
			}
			pb.RegisterVodProcessorServer(server, fake)
			pb.RegisterAiProcessorServer(server, fake)
			go func() { _ = server.Serve(listener) }()
			defer server.Stop()
			pool, err := ai.NewPool([]string{listener.Addr().String()}, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			s := testService(t)
			s.Config.WorkerGPUs = map[string]string{listener.Addr().String(): "GPU-0"}
			e := NewEngine(s, pool, idleGRPCWorker{GRPCWorker{pool}}, fakeMemory{})
			ctx, cancel := context.WithCancel(context.Background())
			defer func() { cancel(); e.wg.Wait() }()
			j := createJob(t, s, "queued")
			if err := e.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			e.wg.Wait()
			current, err := s.store.Load().Get(ctx, j.ID, j.UserID)
			if err != nil {
				t.Fatal(err)
			}
			if tc == "INPUT_TOO_LARGE" {
				if current.Status != "failed" || current.FailureCode != "upload_too_large" {
					t.Fatal(current)
				}
				assertPublicError(t, request(t, handler(s), "GET", "/vod/jobs/"+j.ID.String()+"/result", j.UserID, nil, ""), 413, "upload_too_large")
				detail := request(t, handler(s), "GET", "/vod/jobs/"+j.ID.String(), j.UserID, nil, "")
				if detail.Code != 200 || !strings.Contains(detail.Body.String(), `"code":"upload_too_large"`) || strings.Contains(detail.Body.String(), "private") {
					t.Fatal(detail.Body.String())
				}
			} else {
				if current.Status != "paused" || current.FailureCode != "" || current.CompletedAt != nil || !current.QueuedAt.Equal(j.QueuedAt.Truncate(time.Microsecond)) {
					t.Fatal(current)
				}
				assertPublicError(t, request(t, handler(s), "GET", "/vod/jobs/"+j.ID.String()+"/result", j.UserID, nil, ""), 409, "result_not_ready")
				fake.mu.Lock()
				fake.startError = nil
				fake.start = nil
				fake.mu.Unlock()
				if err := e.Tick(ctx); err != nil {
					t.Fatal(err)
				}
				eventually(t, func() bool { fake.mu.Lock(); defer fake.mu.Unlock(); return fake.start != nil })
			}
		})
	}
}
