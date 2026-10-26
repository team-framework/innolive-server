package vod

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"math"
	"os"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	pb "inno-live-server/api/gen/aiv1"
	"inno-live-server/internal/ai"
)

// Engine has one PostgreSQL advisory-lock leader across server slots. Local pool
// live claims preempt leases without waiting for this scheduler or any RPC.
type Engine struct {
	Service *Service
	Pool    *ai.Pool
	Worker  Worker
	Memory  MemoryProbe
	mu      sync.Mutex
	running map[string]bool
	wg      sync.WaitGroup
}

func NewEngine(s *Service, p *ai.Pool, w Worker, m MemoryProbe) *Engine {
	return &Engine{Service: s, Pool: p, Worker: w, Memory: m, running: map[string]bool{}}
}

const leaderKey int64 = 52120261026

func (e *Engine) Run(ctx context.Context) {
	if !e.Service.Config.Enabled {
		return
	}
	db := e.Service.store.Load()
	if db == nil {
		return
	}
	sqlDB, err := db.DB.DB()
	if err != nil {
		return
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		conn, err := sqlDB.Conn(ctx)
		if err == nil {
			var locked bool
			err = conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", leaderKey).Scan(&locked)
			if err == nil && locked {
				e.lead(ctx, conn)
			}
			// A connection with a possibly held advisory lock must not return to the pool.
			unlockCtx, unlockCancel := context.WithTimeout(context.Background(), 3*time.Second)
			if _, unlockErr := conn.ExecContext(unlockCtx, "SELECT pg_advisory_unlock($1)", leaderKey); unlockErr != nil {
				_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			}
			unlockCancel()
			_ = conn.Close()
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (e *Engine) lead(parent context.Context, conn *sql.Conn) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	defer func() { cancel(); e.wg.Wait() }()
	e.wg.Add(1)
	go func() { defer e.wg.Done(); e.Service.RunSweeper(ctx) }()
	if e.Pool == nil {
		<-ctx.Done()
		return
	}
	// Resume only after every old worker has acknowledged pause/cancel. An
	// unreachable process may still use the GPU, so takeover waits fail-closed.
	if err := e.Reconcile(ctx); err != nil {
		e.Service.Logger.Warn("vod reconciliation pending", "error", err)
		return
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		check, c := context.WithTimeout(ctx, 3*time.Second)
		err := conn.PingContext(check)
		c()
		if err != nil {
			return
		}
		if err := e.Tick(ctx); err != nil {
			e.Service.Logger.Warn("vod scheduling deferred", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:

		}
	}
}
func (e *Engine) Reconcile(ctx context.Context) error {
	db := e.Service.store.Load()
	jobs, err := db.All(ctx)
	if err != nil {
		return err
	}
	known := map[string]Job{}
	for _, j := range jobs {
		known[j.ID.String()] = j
	}
	for _, target := range e.Pool.Targets() {
		if e.Service.Config.WorkerGPUs[target.Address] == "" {
			continue
		}
		call, cancel := context.WithTimeout(ctx, 5*time.Second)
		list, err := e.Worker.List(call, target.Address)
		cancel()
		if err != nil {
			return err
		}
		for _, v := range list {
			j, exists := known[v.JobId]
			call, cancel := context.WithTimeout(ctx, 5*time.Second)
			if !exists || j.DeletedAt != nil {
				_, err = e.Worker.Cancel(call, target.Address, v.JobId)
			} else if v.State == pb.VodJobState_VOD_JOB_STATE_PROCESSING {
				v, err = e.Worker.Pause(call, target.Address, v.JobId)
			}
			cancel()
			if err != nil {
				return err
			}
			if exists && j.DeletedAt == nil {
				j.WorkerTarget = target.Address
				if err = e.apply(ctx, &j, v); err != nil {
					return err
				}
				known[v.JobId] = j
			}
		}
	}
	// Explicitly query recorded targets, including ones removed from the pool.
	for _, j := range known {
		if j.WorkerTarget == "" {
			continue
		}
		call, cancel := context.WithTimeout(ctx, 5*time.Second)
		if j.DeletedAt != nil {
			_, err = e.Worker.Cancel(call, j.WorkerTarget, j.ID.String())
			if err == nil {
				j.WorkerTarget = ""
				err = db.Save(ctx, &j)
			}
		} else {
			var v *pb.VodJobStatus
			v, err = e.Worker.Pause(call, j.WorkerTarget, j.ID.String())
			if err == nil {
				err = e.apply(ctx, &j, v)
			} else if status.Code(err) == codes.NotFound {
				j.Status = "paused"
				j.WorkerTarget = ""
				j.UpdatedAt = e.Service.Now()
				err = db.Save(ctx, &j)
			}
		}
		cancel()
		if err != nil {
			return err
		}
	}
	return nil
}
func (e *Engine) Tick(ctx context.Context) error {
	db := e.Service.store.Load()
	jobs, err := db.All(ctx)
	if err != nil {
		return err
	}
	for i := range jobs {
		j := &jobs[i]
		if j.Status == "uploaded" && j.DeletedAt == nil {
			j.Status = "queued"
			j.UpdatedAt = e.Service.Now()
			if err := db.Save(ctx, j); err != nil && !errors.Is(err, ErrConflict) {
				return err
			}
		}
	}
	// Cancel jobs deleted while no local runner exists (including queued jobs).
	for _, j := range jobs {
		if j.DeletedAt == nil || j.WorkerTarget == "" {
			continue
		}
		e.mu.Lock()
		running := e.running[j.ID.String()]
		e.mu.Unlock()
		if running {
			continue
		}
		call, cancel := context.WithTimeout(ctx, 3*time.Second)
		_, err := e.Worker.Cancel(call, j.WorkerTarget, j.ID.String())
		cancel()
		if err == nil {
			j.WorkerTarget = ""
			if err = db.Save(ctx, &j); err != nil && !errors.Is(err, ErrConflict) {
				return err
			}
		}
	}
	call, cancel := context.WithTimeout(ctx, 2*time.Second)
	free, err := e.availableMemory(call)
	cancel()
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if j.DeletedAt != nil || (j.Status != "queued" && j.Status != "paused") {
			continue
		}
		e.mu.Lock()
		if e.running[j.ID.String()] {
			e.mu.Unlock()
			continue
		}
		lease := e.Pool.ReserveVOD(e.Service.Config.WorkerGPUs, free, e.Service.Config.MinFreeMiB, e.Service.Config.Concurrency)
		if lease == nil {
			e.mu.Unlock()
			break
		}
		if j.Status == "paused" {
			j.Status = "queued"
			j.UpdatedAt = e.Service.Now()
			if err := db.Save(ctx, &j); err != nil {
				lease.Release()
				e.mu.Unlock()
				continue
			}
		}
		e.running[j.ID.String()] = true
		e.wg.Add(1)
		e.mu.Unlock()
		go func(j Job) {
			defer e.wg.Done()
			defer lease.Release()
			defer func() { e.mu.Lock(); delete(e.running, j.ID.String()); e.mu.Unlock() }()
			e.runJob(ctx, j, lease)
		}(j)
	}
	return nil
}
func (e *Engine) runJob(ctx context.Context, j Job, lease *ai.VODLease) {
	// This control path remains responsive even if Start, a database write or
	// telemetry is slow. Pause is idempotent; snapshots are persisted by the
	// single runner below. A NotFound during admission is retried until Start
	// has actually admitted the job or the runner exits.
	interruptCtx, stopInterrupt := context.WithCancel(ctx)
	interruptDone := make(chan struct{})
	go func(id string) { defer close(interruptDone); e.preempt(interruptCtx, lease, id) }(j.ID.String())
	defer func() { stopInterrupt(); <-interruptDone }()

	db := e.Service.store.Load()
	// Persist target before admission: restart reconciliation can locate a job
	// even if the Start response or the subsequent DB write was lost.
	j.Status = "processing"
	j.WorkerTarget = lease.Target
	j.UpdatedAt = e.Service.Now()
	if err := db.Save(ctx, &j); err != nil {
		return
	}
	call, cancel := context.WithTimeout(ctx, 10*time.Second)
	whitelist, err := e.Worker.Whitelist(call, "user:"+j.UserID.String())
	cancel()
	if err != nil {
		e.failUnstarted(ctx, &j)
		return
	}
	select {
	case <-lease.Preempt:
		e.failUnstarted(ctx, &j)
		return
	default:
	}
	req := &pb.StartVodJobRequest{JobId: j.ID.String(), InputPath: e.Service.Path(j, "original"), OutputPath: e.Service.Path(j, "result"), WorkDir: e.Service.Path(j, "work"), WhitelistSource: &pb.StartVodJobRequest_Whitelist{Whitelist: whitelist}, AnonymizationMode: pb.FaceAnonymizationMode_FACE_ANONYMIZATION_MODE_BLUR, Resume: j.CheckpointToken != "", CheckpointToken: j.CheckpointToken, SegmentFrames: 300}
	call, cancel = context.WithTimeout(ctx, 10*time.Second)
	v, err := e.Worker.Start(call, lease.Target, req)
	cancel()
	if status.Code(err) == codes.ResourceExhausted || status.Code(err) == codes.FailedPrecondition || status.Code(err) == codes.InvalidArgument || status.Code(err) == codes.Unimplemented {
		if status.Code(err) != codes.ResourceExhausted {
			e.Service.Finish(&j, "failed")
			j.FailureCode = "processing_failed"
			j.WorkerTarget = ""
			_ = db.Save(ctx, &j)
		} else {
			e.failUnstarted(ctx, &j)
		}
		return
	}
	if err == nil {
		if saveErr := e.apply(ctx, &j, v); saveErr == nil && j.Status != "processing" {
			return
		}
	}
	// Ambiguous admission errors are reconciled by Get/Pause; never start a second
	// copy merely because the network lost the successful Start response.
	watchCtx, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	updates := make(chan *pb.VodJobStatus, 8)
	watchDone := make(chan error, 1)
	jobID := j.ID.String()
	go func(done chan<- error) {
		done <- e.Worker.Watch(watchCtx, lease.Target, jobID, func(v *pb.VodJobStatus) error {
			select {
			case updates <- v:
				return nil
			case <-watchCtx.Done():
				return watchCtx.Err()
			}
		})
	}(watchDone)
	defer func() {
		stopWatch()
		if watchDone != nil {
			<-watchDone
		}
	}()
	poll := time.NewTicker(500 * time.Millisecond)
	defer poll.Stop()
	preempt := lease.Preempt
	yield := false
	for {
		select {
		case <-ctx.Done():
			call, c := context.WithTimeout(context.Background(), 5*time.Second)
			v, err := e.Worker.Pause(call, lease.Target, j.ID.String())
			if err == nil {
				_ = e.refreshApply(call, &j, v)
			}
			c()
			return
		case <-preempt:
			yield = true
			preempt = nil
		case v := <-updates:
			if err := e.refreshApply(ctx, &j, v); err == nil && j.Status != "processing" {
				return
			}
		case <-watchDone:
			watchDone = nil
		case <-poll.C:
		}

		// Live preemption reaches the worker even while the database or GPU
		// telemetry is unavailable. Neither is on the pause control path.
		if yield {
			call, c := context.WithTimeout(ctx, 3*time.Second)
			paused, pauseErr := e.Worker.Pause(call, lease.Target, jobID)
			c()
			if pauseErr == nil {
				if err := e.refreshApply(ctx, &j, paused); err == nil && j.Status != "processing" {
					return
				}
			}
		}
		current, err := db.Get(ctx, j.ID, j.UserID)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			// Get hides deleted jobs. Confirm the tombstone separately; DB outages do not
			// authorize cancellation or filesystem deletion.
			all, readErr := db.All(ctx)
			if readErr != nil {
				continue
			}
			found := false
			for _, row := range all {
				if row.ID == j.ID {
					current = row
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		j = current
		if j.DeletedAt != nil {
			call, c := context.WithTimeout(ctx, 3*time.Second)
			_, err = e.Worker.Cancel(call, lease.Target, j.ID.String())
			c()
			if err == nil {
				j.WorkerTarget = ""
				_ = db.Save(ctx, &j)
				return
			}
			continue
		}
		if yield {
			continue
		}
		// Memory pressure yields existing work as well as preventing new admissions.
		call, c := context.WithTimeout(ctx, 2*time.Second)
		free, memErr := e.availableMemory(call)
		c()
		if memErr != nil || free[lease.GPU] < e.Service.Config.MinFreeMiB {
			yield = true
		}
		call, c = context.WithTimeout(ctx, 3*time.Second)
		if yield {
			v, err = e.Worker.Pause(call, lease.Target, j.ID.String())
		} else {
			v, err = e.Worker.Get(call, lease.Target, j.ID.String())
		}
		c()
		if status.Code(err) == codes.NotFound {
			j.Status = "paused"
			j.WorkerTarget = ""
			j.UpdatedAt = e.Service.Now()
			_ = db.Save(ctx, &j)
			return
		}
		if err == nil {
			if err = e.refreshApply(ctx, &j, v); err == nil && j.Status != "processing" {
				return
			}
		}
	}
}
func (e *Engine) failUnstarted(ctx context.Context, j *Job) {
	j.Status = "paused"
	j.WorkerTarget = ""
	j.UpdatedAt = e.Service.Now()
	_ = e.Service.store.Load().Save(ctx, j)
}
func (e *Engine) refreshApply(ctx context.Context, j *Job, v *pb.VodJobStatus) error {
	current, err := e.Service.store.Load().Get(ctx, j.ID, j.UserID)
	if err != nil {
		return err
	}
	*j = current
	return e.apply(ctx, j, v)
}
func (e *Engine) apply(ctx context.Context, j *Job, v *pb.VodJobStatus) error {
	if v == nil || v.JobId != j.ID.String() {
		return fmt.Errorf("invalid VOD worker snapshot")
	}
	if j.DeletedAt != nil || j.Terminal() {
		return nil
	}
	p := v.GetProgress()
	if p != nil {
		j.FramesDone = p.FramesDone
		j.FramesTotal = p.FramesTotal
		j.SegmentsDone = p.SegmentsDone
		if p.ProcessingFps > 0 && !math.IsInf(p.ProcessingFps, 0) && !math.IsNaN(p.ProcessingFps) {
			j.ProcessingFPS = p.ProcessingFps
		}
		// API progress counts durable segments, unlike the worker fraction which
		// also includes the current, discardable segment. Keep the high-water
		// mark across estimated frame count changes and checkpoint replay.
		if p.FramesTotal > 0 {
			committed := min(uint64(p.SegmentsDone)*300, p.FramesTotal)
			j.Progress = max(j.Progress, min(99, int(float64(committed)*100/float64(p.FramesTotal))))
		}
	}
	if v.CheckpointToken != "" {
		j.CheckpointToken = v.CheckpointToken
	}
	switch v.State {
	case pb.VodJobState_VOD_JOB_STATE_PROCESSING:
		j.Status = "processing"
	case pb.VodJobState_VOD_JOB_STATE_PAUSED:
		j.Status = "paused"
		j.WorkerTarget = ""
	case pb.VodJobState_VOD_JOB_STATE_DONE:
		info, err := os.Lstat(e.Service.Path(*j, "result"))
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			e.Service.Finish(j, "failed")
			j.FailureCode = "storage_failed"
		} else {
			e.Service.Finish(j, "done")
		}
		j.WorkerTarget = ""
	case pb.VodJobState_VOD_JOB_STATE_FAILED:
		e.Service.Finish(j, "failed")
		j.FailureCode = "processing_failed"
		j.WorkerTarget = ""
	case pb.VodJobState_VOD_JOB_STATE_CANCELLED:
		e.Service.Finish(j, "cancelled")
		j.WorkerTarget = ""
	default:
		return fmt.Errorf("unknown VOD worker state")
	}
	j.UpdatedAt = e.Service.Now()
	err := e.Service.store.Load().Save(ctx, j)
	if err == nil && j.Terminal() {
		e.Service.WakeSweep()
	}
	return err
}

func (e *Engine) availableMemory(ctx context.Context) (map[string]int64, error) {
	free, err := e.Memory.FreeMiB(ctx)
	if err != nil {
		return nil, err
	}
	if free == nil {
		free = map[string]int64{}
	}
	// A new unmapped replacement slot may share any GPU. Suspend VOD until
	// operators provide its physical identity; live routing remains unaffected.
	for _, target := range e.Pool.Targets() {
		gpu := e.Service.Config.WorkerGPUs[target.Address]
		if gpu == "" {
			return nil, fmt.Errorf("missing VOD GPU mapping")
		}
		live, err := e.Worker.LiveStreams(ctx, target.Address)
		if err != nil || live > 0 {
			free[gpu] = -1
		}
	}
	return free, nil
}

func (e *Engine) preempt(ctx context.Context, lease *ai.VODLease, id string) {
	select {
	case <-ctx.Done():
		return
	case <-lease.Preempt:
	}
	for ctx.Err() == nil {
		call, cancel := context.WithTimeout(ctx, 3*time.Second)
		v, err := e.Worker.Pause(call, lease.Target, id)
		cancel()
		if err == nil && v != nil && v.State != pb.VodJobState_VOD_JOB_STATE_PROCESSING && v.State != pb.VodJobState_VOD_JOB_STATE_UNSPECIFIED {
			return
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
