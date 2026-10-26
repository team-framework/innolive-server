package vod

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"strconv"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	pb "inno-live-server/api/gen/aiv1"
	"inno-live-server/internal/ai"
)

// Worker is the interface-A contract. Callers own deadlines, cancellation and
// checkpoint persistence. Whitelist embeddings are never written to disk.
type Worker interface {
	LiveStreams(context.Context, string) (uint64, error)
	Start(context.Context, string, *pb.StartVodJobRequest) (*pb.VodJobStatus, error)
	Watch(context.Context, string, string, func(*pb.VodJobStatus) error) error
	Get(context.Context, string, string) (*pb.VodJobStatus, error)
	Pause(context.Context, string, string) (*pb.VodJobStatus, error)
	Cancel(context.Context, string, string) (*pb.VodJobStatus, error)
	List(context.Context, string) ([]*pb.VodJobStatus, error)
	Whitelist(context.Context, string) (*pb.VodWhitelist, error)
}
type GRPCWorker struct{ Pool *ai.Pool }

func (w GRPCWorker) client(target string) (pb.VodProcessorClient, error) {
	c := w.Pool.Target(target)
	if c == nil {
		return nil, status.Error(codes.Unavailable, "worker removed")
	}
	return c.VOD(), nil
}
func (w GRPCWorker) Start(ctx context.Context, t string, r *pb.StartVodJobRequest) (*pb.VodJobStatus, error) {
	c, e := w.client(t)
	if e != nil {
		return nil, e
	}
	return c.StartVodJob(ctx, r)
}
func (w GRPCWorker) Get(ctx context.Context, t, id string) (*pb.VodJobStatus, error) {
	c, e := w.client(t)
	if e != nil {
		return nil, e
	}
	return c.GetVodJob(ctx, &pb.VodJobRef{JobId: id})
}
func (w GRPCWorker) Pause(ctx context.Context, t, id string) (*pb.VodJobStatus, error) {
	c, e := w.client(t)
	if e != nil {
		return nil, e
	}
	return c.PauseVodJob(ctx, &pb.PauseVodJobRequest{JobId: id})
}
func (w GRPCWorker) Cancel(ctx context.Context, t, id string) (*pb.VodJobStatus, error) {
	c, e := w.client(t)
	if e != nil {
		return nil, e
	}
	return c.CancelVodJob(ctx, &pb.CancelVodJobRequest{JobId: id, PurgeWorkDir: true})
}
func (w GRPCWorker) List(ctx context.Context, t string) ([]*pb.VodJobStatus, error) {
	c, e := w.client(t)
	if e != nil {
		return nil, e
	}
	r, e := c.ListVodJobs(ctx, &pb.ListVodJobsRequest{})
	if e != nil {
		return nil, e
	}
	return r.Jobs, nil
}
func (w GRPCWorker) Watch(ctx context.Context, t, id string, callback func(*pb.VodJobStatus) error) error {
	c, e := w.client(t)
	if e != nil {
		return e
	}
	stream, e := c.WatchVodJob(ctx, &pb.VodJobRef{JobId: id})
	if e != nil {
		return e
	}
	for {
		r, e := stream.Recv()
		if e == io.EOF {
			return nil
		}
		if e != nil {
			return e
		}
		if e = callback(r); e != nil {
			return e
		}
	}
}
func (w GRPCWorker) Whitelist(ctx context.Context, session string) (*pb.VodWhitelist, error) {
	var last error
	for _, c := range w.Pool.Clients() {
		r, e := c.ExportWhitelist(ctx, session)
		if e == nil {
			return &pb.VodWhitelist{EmbeddingModel: r.EmbeddingModel, Entries: r.Entries}, nil
		}
		last = e
	}
	// Never silently fall back after an export error: it could change resume identity.
	if last == nil {
		last = errors.New("no whitelist source")
	}
	return nil, last
}

type MemoryProbe interface {
	FreeMiB(context.Context) (map[string]int64, error)
}
type NvidiaMemory struct{}

func (NvidiaMemory) FreeMiB(ctx context.Context) (map[string]int64, error) {
	b, e := exec.CommandContext(ctx, "nvidia-smi", "--query-gpu=uuid,memory.free", "--format=csv,noheader,nounits").Output()
	if e != nil {
		return nil, e
	}
	result := map[string]int64{}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		fields := strings.Split(line, ",")
		if len(fields) != 2 {
			return nil, errors.New("invalid GPU telemetry")
		}
		n, e := strconv.ParseInt(strings.TrimSpace(fields[1]), 10, 64)
		if e != nil || n < 0 {
			return nil, errors.New("invalid GPU memory")
		}
		result[strings.TrimSpace(fields[0])] = n
	}
	return result, nil
}

// ListSessions observes live streams from other server slots as well as this pool.
func (w GRPCWorker) LiveStreams(ctx context.Context, target string) (uint64, error) {
	c := w.Pool.Target(target)
	if c == nil {
		return 0, status.Error(codes.Unavailable, "worker removed")
	}
	response, err := c.ListSessions(ctx)
	if err != nil {
		return 0, err
	}
	var count uint64
	for _, session := range response.Sessions {
		count += uint64(session.ActiveStreamCount)
	}
	return count, nil
}
