package vod

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	pb "inno-live-server/api/gen/aiv1"
	"inno-live-server/internal/ai"
)

type fakeMemory struct{}

func (fakeMemory) FreeMiB(context.Context) (map[string]int64, error) {
	return map[string]int64{"GPU-0": 8192, "GPU-1": 8192}, nil
}

type fakeWorker struct {
	mu         sync.Mutex
	jobs       map[string]*pb.VodJobStatus
	starts     []*pb.StartVodJobRequest
	pauseGate  <-chan struct{}
	startErr   error
	remoteLive map[string]uint64
}

func newFakeWorker() *fakeWorker { return &fakeWorker{jobs: map[string]*pb.VodJobStatus{}} }
func (f *fakeWorker) Start(_ context.Context, target string, r *pb.StartVodJobRequest) (*pb.VodJobStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return nil, f.startErr
	}
	f.starts = append(f.starts, proto.Clone(r).(*pb.StartVodJobRequest))
	v := &pb.VodJobStatus{JobId: r.JobId, State: pb.VodJobState_VOD_JOB_STATE_PROCESSING, CheckpointToken: r.CheckpointToken, Progress: &pb.VodProgress{FramesDone: 300, FramesTotal: 1800, Fraction: 1.0 / 6, ProcessingFps: 30}, WorkerTarget: target}
	f.jobs[r.JobId] = v
	return proto.Clone(v).(*pb.VodJobStatus), nil
}
func (f *fakeWorker) Get(_ context.Context, _ string, id string) (*pb.VodJobStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v := f.jobs[id]
	if v == nil {
		return nil, status.Error(codes.NotFound, "unknown")
	}
	return proto.Clone(v).(*pb.VodJobStatus), nil
}
func (f *fakeWorker) Watch(ctx context.Context, _ string, _ string, _ func(*pb.VodJobStatus) error) error {
	<-ctx.Done()
	return ctx.Err()
}
func (f *fakeWorker) Pause(ctx context.Context, _ string, id string) (*pb.VodJobStatus, error) {
	if f.pauseGate != nil {
		select {
		case <-f.pauseGate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	v := f.jobs[id]
	if v == nil {
		return nil, status.Error(codes.NotFound, "unknown")
	}
	v.State = pb.VodJobState_VOD_JOB_STATE_PAUSED
	v.CheckpointToken = "opaque-checkpoint-300"
	v.Progress.SegmentsDone = 1
	return proto.Clone(v).(*pb.VodJobStatus), nil
}
func (f *fakeWorker) Cancel(_ context.Context, _ string, id string) (*pb.VodJobStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v := &pb.VodJobStatus{JobId: id, State: pb.VodJobState_VOD_JOB_STATE_CANCELLED}
	f.jobs[id] = v
	return proto.Clone(v).(*pb.VodJobStatus), nil
}
func (f *fakeWorker) List(_ context.Context, target string) ([]*pb.VodJobStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*pb.VodJobStatus
	for _, v := range f.jobs {
		if v.WorkerTarget == target {
			out = append(out, proto.Clone(v).(*pb.VodJobStatus))
		}
	}
	return out, nil
}
func (f *fakeWorker) Whitelist(context.Context, string) (*pb.VodWhitelist, error) {
	return &pb.VodWhitelist{EmbeddingModel: "test-model", Entries: []*pb.WhitelistEmbedding{{EntryId: "registered-person", Values: []float32{1, 0}}}}, nil
}
func eventually(t *testing.T, check func() bool) {
	t.Helper()
	until := time.Now().Add(6 * time.Second)
	for time.Now().Before(until) {
		if check() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition timed out")
}
func setupEngine(t *testing.T) (*Service, *Engine, *fakeWorker, context.Context) {
	t.Helper()
	s := testService(t)
	p, _ := ai.NewPool([]string{"a:1", "b:1", "c:1"}, time.Second)
	s.Config.WorkerGPUs = map[string]string{"a:1": "GPU-0", "b:1": "GPU-0", "c:1": "GPU-1"}
	f := newFakeWorker()
	e := NewEngine(s, p, f, fakeMemory{})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); e.wg.Wait(); _ = p.Close() })
	return s, e, f, ctx
}
func TestSchedulerLivePreemptionCheckpointResume(t *testing.T) {
	s, e, f, ctx := setupEngine(t)
	gate := make(chan struct{})
	f.pauseGate = gate
	j := createJob(t, s, "uploaded")
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.starts) == 1 })
	done := make(chan func(), 1)
	go func() { _, release := e.Pool.Acquire(false); done <- release }()
	var release func()
	select {
	case release = <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("live blocked on Pause RPC")
	}
	close(gate)
	eventually(t, func() bool {
		current, _ := s.store.Load().Get(ctx, j.ID, j.UserID)
		return current.Status == "paused" && current.CheckpointToken == "opaque-checkpoint-300" && current.SegmentsDone == 1
	})
	release()
	eventually(t, func() bool { e.mu.Lock(); defer e.mu.Unlock(); return len(e.running) == 0 })
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.starts) == 2 })
	f.mu.Lock()
	last := f.starts[1]
	f.mu.Unlock()
	if !last.Resume || last.CheckpointToken != "opaque-checkpoint-300" || len(last.GetWhitelist().Entries) != 1 {
		t.Fatalf("resume lost checkpoint or whitelist: %v", last)
	}
}
func TestSchedulerGlobalAndGPUConcurrency(t *testing.T) {
	s, e, f, ctx := setupEngine(t)
	s.Config.Concurrency = 2
	for range 4 {
		createJob(t, s, "queued")
	}
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.starts) == 2 })
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.starts) != 2 {
		t.Fatal("global concurrency exceeded")
	}
	gpus := map[string]bool{}
	for _, v := range f.jobs {
		g := s.Config.WorkerGPUs[v.WorkerTarget]
		if gpus[g] {
			t.Fatal("two jobs on one GPU")
		}
		gpus[g] = true
	}
}
func TestSchedulerResourceExhaustedRequeues(t *testing.T) {
	s, e, f, ctx := setupEngine(t)
	f.startErr = status.Error(codes.ResourceExhausted, "busy")
	j := createJob(t, s, "queued")
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		current, _ := s.store.Load().Get(ctx, j.ID, j.UserID)
		return current.Status == "paused" && current.WorkerTarget == ""
	})
}
func TestSchedulerReconcilesWorkerRestart(t *testing.T) {
	s, e, f, ctx := setupEngine(t)
	j := createJob(t, s, "processing")
	j.WorkerTarget = "a:1"
	j.CheckpointToken = "old"
	saveJob(t, s, &j)
	f.jobs[j.ID.String()] = &pb.VodJobStatus{JobId: j.ID.String(), State: pb.VodJobState_VOD_JOB_STATE_PROCESSING, WorkerTarget: "a:1", Progress: &pb.VodProgress{FramesDone: 300, FramesTotal: 1800}}
	if err := e.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	current, _ := s.store.Load().Get(ctx, j.ID, j.UserID)
	if current.Status != "paused" || current.CheckpointToken != "opaque-checkpoint-300" {
		t.Fatal(current)
	}
}
func TestWorkerStatusTransitionsAndMonotonicProgress(t *testing.T) {
	s, e, _, ctx := setupEngine(t)
	j := createJob(t, s, "processing")
	v := &pb.VodJobStatus{JobId: j.ID.String(), State: pb.VodJobState_VOD_JOB_STATE_PROCESSING, Progress: &pb.VodProgress{FramesDone: 900, FramesTotal: 1800, SegmentsDone: 3, Fraction: .5, ProcessingFps: 30}}
	if err := e.apply(ctx, &j, v); err != nil {
		t.Fatal(err)
	}
	v.State = pb.VodJobState_VOD_JOB_STATE_PAUSED
	v.Progress.Fraction = .3
	v.CheckpointToken = "last-durable"
	if err := e.apply(ctx, &j, v); err != nil {
		t.Fatal(err)
	}
	if j.Progress != 50 || j.Status != "paused" {
		t.Fatal(j)
	}
	writeFile(t, s.Path(j, "result"))
	v.State = pb.VodJobState_VOD_JOB_STATE_DONE
	if err := e.apply(ctx, &j, v); err != nil {
		t.Fatal(err)
	}
	if j.Progress != 100 || j.CompletedAt == nil || j.ResultExpiresAt == nil {
		t.Fatal(j)
	}
	v.State = pb.VodJobState_VOD_JOB_STATE_PROCESSING
	if err := e.apply(ctx, &j, v); err != nil || j.Status != "done" {
		t.Fatal("terminal state resurrected")
	}
}
func TestQueuePositionAndETA(t *testing.T) {
	s := testService(t)
	a := createJob(t, s, "processing")
	a.ProcessingFPS = 30
	a.FramesDone = 900
	saveJob(t, s, &a)
	b := createJob(t, s, "queued")
	c := createJob(t, s, "paused")
	jobs, _ := s.store.Load().All(context.Background())
	v := s.View(c, jobs)
	if v.QueuePosition == nil || *v.QueuePosition != 2 || v.ETA == nil || *v.ETA != 150 {
		t.Fatalf("queue=%v ETA=%v", v.QueuePosition, v.ETA)
	}
	v = s.View(b, []Job{b, c})
	if v.ETA != nil {
		t.Fatal("invented ETA without throughput")
	}
}
func TestMigrationIsAdditiveAndRollback(t *testing.T) {
	s := testService(t)
	db := s.store.Load().DB
	if err := db.Exec("CREATE TABLE existing_sentinel (id integer PRIMARY KEY); INSERT INTO existing_sentinel VALUES (42)").Error; err != nil {
		t.Fatal(err)
	}
	up, err := os.ReadFile("../database/migration/sql/000018_create_vod_jobs.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if string(up) != schemaSQL {
		t.Fatal("auto/versioned drift")
	}
	if err = db.Exec(string(up)).Error; err != nil {
		t.Fatal(err)
	}
	down, err := os.ReadFile("../database/migration/sql/000018_create_vod_jobs.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Exec(string(down)).Error; err != nil {
		t.Fatal(err)
	}
	var id int
	if err = db.Raw("SELECT id FROM existing_sentinel").Scan(&id).Error; err != nil || id != 42 {
		t.Fatal("existing table changed", err)
	}
	if db.Migrator().HasTable(&Job{}) {
		t.Fatal("rollback failed")
	}
	if err = AutoMigrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	j := createJob(t, s, "queued")
	stale := j
	j.Status = "paused"
	saveJob(t, s, &j)
	if err = s.store.Load().Save(context.Background(), &stale); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
}

func (f *fakeWorker) LiveStreams(_ context.Context, target string) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.remoteLive[target], nil
}

func TestSchedulerObservesLiveFromOtherServerSlot(t *testing.T) {
	s, e, f, ctx := setupEngine(t)
	j := createJob(t, s, "queued")
	f.remoteLive = map[string]uint64{"a:1": 1, "c:1": 1}
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	starts := len(f.starts)
	f.remoteLive = map[string]uint64{}
	f.mu.Unlock()
	if starts != 0 {
		t.Fatal("ignored other slot live streams")
	}
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.starts) == 1 })
	f.mu.Lock()
	f.remoteLive["a:1"] = 1
	f.mu.Unlock()
	eventually(t, func() bool { current, _ := s.store.Load().Get(ctx, j.ID, j.UserID); return current.Status == "paused" })
}

func TestSchedulerPostgresLeaderExclusive(t *testing.T) {
	s, e, f, _ := setupEngine(t)
	createJob(t, s, "queued")
	pool2, err := ai.NewPool([]string{"a:1", "b:1", "c:1"}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer pool2.Close()
	other := NewEngine(s, pool2, f, fakeMemory{})
	ctx, cancel := context.WithCancel(context.Background())
	done1, done2 := make(chan struct{}), make(chan struct{})
	go func() { defer close(done1); e.Run(ctx) }()
	go func() { defer close(done2); other.Run(ctx) }()
	defer func() { cancel(); <-done1; <-done2 }()
	eventually(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.starts) == 1 })
	time.Sleep(1200 * time.Millisecond)
	f.mu.Lock()
	starts := len(f.starts)
	f.mu.Unlock()
	if starts != 1 {
		t.Fatalf("multiple leaders admitted %d jobs", starts)
	}
}
