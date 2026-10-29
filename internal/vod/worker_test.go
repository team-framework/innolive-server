package vod

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	pb "inno-live-server/api/gen/aiv1"
	"inno-live-server/internal/ai"
)

type rpcWorker struct {
	pb.UnimplementedVodProcessorServer
	pb.UnimplementedAiProcessorServer
	mu           sync.Mutex
	start        *pb.StartVodJobRequest
	purged       bool
	startFailure string
	startError   error
}

func (f *rpcWorker) StartVodJob(_ context.Context, r *pb.StartVodJobRequest) (*pb.VodJobStatus, error) {
	f.mu.Lock()
	f.start = proto.Clone(r).(*pb.StartVodJobRequest)
	defer f.mu.Unlock()
	if f.startError != nil {
		return nil, f.startError
	}
	if f.startFailure != "" {
		return &pb.VodJobStatus{JobId: r.JobId, State: pb.VodJobState_VOD_JOB_STATE_FAILED, ErrorCode: f.startFailure, ErrorMessage: "private worker path"}, nil
	}
	return &pb.VodJobStatus{JobId: r.JobId, State: pb.VodJobState_VOD_JOB_STATE_PROCESSING, CheckpointToken: r.CheckpointToken}, nil
}
func (f *rpcWorker) WatchVodJob(r *pb.VodJobRef, stream grpc.ServerStreamingServer[pb.VodJobStatus]) error {
	return stream.Send(&pb.VodJobStatus{JobId: r.JobId, State: pb.VodJobState_VOD_JOB_STATE_PAUSED, CheckpointToken: "checkpoint", Progress: &pb.VodProgress{FramesDone: 300, SegmentsDone: 1}})
}
func (f *rpcWorker) GetVodJob(_ context.Context, r *pb.VodJobRef) (*pb.VodJobStatus, error) {
	return &pb.VodJobStatus{JobId: r.JobId, State: pb.VodJobState_VOD_JOB_STATE_PROCESSING}, nil
}
func (f *rpcWorker) PauseVodJob(_ context.Context, r *pb.PauseVodJobRequest) (*pb.VodJobStatus, error) {
	return &pb.VodJobStatus{JobId: r.JobId, State: pb.VodJobState_VOD_JOB_STATE_PAUSED, CheckpointToken: "checkpoint"}, nil
}
func (f *rpcWorker) CancelVodJob(_ context.Context, r *pb.CancelVodJobRequest) (*pb.VodJobStatus, error) {
	f.mu.Lock()
	f.purged = r.PurgeWorkDir
	f.mu.Unlock()
	return &pb.VodJobStatus{JobId: r.JobId, State: pb.VodJobState_VOD_JOB_STATE_CANCELLED}, nil
}
func (f *rpcWorker) ListVodJobs(context.Context, *pb.ListVodJobsRequest) (*pb.ListVodJobsResponse, error) {
	return &pb.ListVodJobsResponse{Jobs: []*pb.VodJobStatus{{JobId: "job", State: pb.VodJobState_VOD_JOB_STATE_PAUSED}}}, nil
}
func (f *rpcWorker) ExportWhitelist(_ context.Context, r *pb.ExportWhitelistRequest) (*pb.ExportWhitelistResponse, error) {
	return &pb.ExportWhitelistResponse{SessionId: r.SessionId, EmbeddingModel: "adaface-test", Entries: []*pb.WhitelistEmbedding{{EntryId: "face", Values: []float32{1, 0}}}}, nil
}
func TestGRPCAdapterContractAllMethods(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	fake := &rpcWorker{}
	pb.RegisterVodProcessorServer(server, fake)
	pb.RegisterAiProcessorServer(server, fake)
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	target := listener.Addr().String()
	pool, err := ai.NewPool([]string{target}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	worker := GRPCWorker{pool}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if n, err := worker.LiveStreams(ctx, target); err != nil || n != 2 {
		t.Fatal("cross-slot live count", n, err)
	}
	whitelist, err := worker.Whitelist(ctx, "user:id")
	if err != nil {
		t.Fatal(err)
	}
	req := &pb.StartVodJobRequest{JobId: "job", InputPath: "/root/job/input.mp4", OutputPath: "/root/job/output.mp4", WorkDir: "/root/job/work", Resume: true, CheckpointToken: "checkpoint", WhitelistSource: &pb.StartVodJobRequest_Whitelist{Whitelist: whitelist}}
	if _, err = worker.Start(ctx, target, req); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	same := proto.Equal(fake.start, req)
	fake.mu.Unlock()
	if !same {
		t.Fatal("Start contract changed")
	}
	seen := false
	if err = worker.Watch(ctx, target, "job", func(v *pb.VodJobStatus) error {
		seen = v.CheckpointToken == "checkpoint" && v.Progress.FramesDone == 300
		return nil
	}); err != nil || !seen {
		t.Fatal("Watch callback", err)
	}
	if _, err = worker.Get(ctx, target, "job"); err != nil {
		t.Fatal(err)
	}
	if v, err := worker.Pause(ctx, target, "job"); err != nil || v.State != pb.VodJobState_VOD_JOB_STATE_PAUSED {
		t.Fatal(err)
	}
	if _, err = worker.Cancel(ctx, target, "job"); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	purged := fake.purged
	fake.mu.Unlock()
	if !purged {
		t.Fatal("cancel did not purge work")
	}
	if v, err := worker.List(ctx, target); err != nil || len(v) != 1 {
		t.Fatal(err)
	}
}

func (f *rpcWorker) ListSessions(context.Context, *pb.ListSessionsRequest) (*pb.ListSessionsResponse, error) {
	return &pb.ListSessionsResponse{Sessions: []*pb.SessionInfo{{SessionId: "live-other-slot", ActiveStreamCount: 2}}}, nil
}
