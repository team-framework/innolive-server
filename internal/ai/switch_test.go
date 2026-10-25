package ai

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	aiv1 "inno-live-server/api/gen/aiv1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// taggingAIServer는 프레임 앞에 자기 이름을 붙여 돌려준다. 출력 프레임이 어느 워커에서
// 왔는지 가린다.
type taggingAIServer struct {
	aiv1.UnimplementedAiProcessorServer
	name      string
	errorCode string
	block     chan struct{}
	frames    atomic.Int64
}

func (s *taggingAIServer) ProcessVideo(stream grpc.BidiStreamingServer[aiv1.VideoChunk, aiv1.ProcessedVideoChunk]) error {
	for {
		request, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		s.frames.Add(1)
		if s.block != nil {
			select {
			case <-s.block:
			case <-stream.Context().Done():
				return stream.Context().Err()
			}
		}
		response := &aiv1.ProcessedVideoChunk{Data: []byte(s.name + ":" + string(request.GetData())), Timestamp: request.GetTimestamp(), StatusMessage: "success"}
		if s.errorCode != "" {
			response = &aiv1.ProcessedVideoChunk{Timestamp: request.GetTimestamp(), ErrorCode: s.errorCode}
		}
		if err := stream.Send(response); err != nil {
			return err
		}
	}
}

func newTaggingClient(t *testing.T, server *taggingAIServer) *Client {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	aiv1.RegisterAiProcessorServer(grpcServer, server)
	go grpcServer.Serve(listener)
	t.Cleanup(grpcServer.Stop)

	connection, err := grpc.NewClient(
		"passthrough:///"+server.name,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
	)
	if err != nil {
		t.Fatal(err)
	}
	client := &Client{address: server.name, conn: connection, client: aiv1.NewAiProcessorClient(connection), timeout: time.Second}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// countRelease는 불린 횟수를 세는 release다.
func countRelease() (func(), *atomic.Int64) {
	var calls atomic.Int64
	return func() { calls.Add(1) }, &calls
}

func processTag(t *testing.T, stream *SwitchableStream, timestamp int64) string {
	t.Helper()
	response, err := stream.Process([]byte("frame"), timestamp, 0, 0, "", nil)
	if err != nil {
		t.Fatalf("Process(%d) error = %v", timestamp, err)
	}
	if response.GetTimestamp() != timestamp {
		t.Fatalf("Process(%d) timestamp = %d", timestamp, response.GetTimestamp())
	}
	tag, _, _ := strings.Cut(string(response.GetData()), ":")
	return tag
}

// switchWhileStreaming은 프레임을 계속 보내며 SwitchTo의 결과를 기다리고, 그동안의
// 출력 워커 순서를 돌려준다.
func switchWhileStreaming(t *testing.T, ctx context.Context, stream *SwitchableStream, target *Client, release func()) ([]string, error) {
	t.Helper()
	result := make(chan error, 1)
	go func() { result <- stream.SwitchTo(ctx, target, release) }()
	var tags []string
	for timestamp := int64(100); ; timestamp++ {
		tags = append(tags, processTag(t, stream, timestamp))
		select {
		case err := <-result:
			return tags, err
		case <-time.After(5 * time.Millisecond):
		}
		if timestamp > 2000 {
			t.Fatal("SwitchTo() did not finish while frames were flowing")
		}
	}
}

// 진행 중인 세션이 다음 프레임부터 새 워커 결과를 내보낸다(#515). 그 전 프레임은 모두
// 지금 워커의 결과다.
func TestSwitchableStreamSwitchesAtFrameBoundary(t *testing.T) {
	serverA, serverB := &taggingAIServer{name: "a"}, &taggingAIServer{name: "b"}
	clientA, clientB := newTaggingClient(t, serverA), newTaggingClient(t, serverB)
	releaseA, releasedA := countRelease()
	releaseB, releasedB := countRelease()
	stream := NewSwitchableStream(context.Background(), clientA, releaseA, "session", false)

	if tag := processTag(t, stream, 1); tag != "a" {
		t.Fatalf("first frame from %q, want a", tag)
	}
	tags, err := switchWhileStreaming(t, context.Background(), stream, clientB, releaseB)
	if err != nil {
		t.Fatalf("SwitchTo() error = %v", err)
	}
	// 전환은 프레임 처리 직전에 일어나므로 SwitchTo가 돌아온 그 프레임부터 b일 수 있다.
	// 한 번 b로 넘어간 뒤 a가 다시 나오면 안 된다.
	switched := false
	for _, tag := range tags {
		switched = switched || tag == "b"
		if tag != "a" && tag != "b" || switched && tag == "a" {
			t.Fatalf("output workers during the switch = %v, want a... then b...", tags)
		}
	}
	if tag := processTag(t, stream, 5000); tag != "b" {
		t.Fatalf("frame after the switch from %q, want b", tag)
	}
	if stream.Target() != "b" {
		t.Fatalf("Target() = %q, want b", stream.Target())
	}
	if got := serverB.frames.Load(); got < DefaultWarmFrames+1 {
		t.Fatalf("worker b saw %d frames, want warm frames plus the switched frame", got)
	}
	if releasedA.Load() != 1 || releasedB.Load() != 0 {
		t.Fatalf("releases a=%d b=%d, want a released once and b still held", releasedA.Load(), releasedB.Load())
	}

	stream.Release()
	stream.Release()
	if releasedB.Load() != 1 {
		t.Fatalf("Release() released b %d times, want once", releasedB.Load())
	}
	if _, err := stream.Process([]byte("frame"), 6000, 0, 0, "", nil); !errors.Is(err, ErrSwitchClosed) {
		t.Fatalf("Process() after Release error = %v, want ErrSwitchClosed", err)
	}
}

// 새 워커가 예열 프레임을 처리하지 못하면 지금 워커를 그대로 쓴다.
func TestSwitchableStreamKeepsCurrentWorkerWhenWarmupFails(t *testing.T) {
	serverA, serverB := &taggingAIServer{name: "a"}, &taggingAIServer{name: "b", errorCode: "INFERENCE_FAILED"}
	clientA, clientB := newTaggingClient(t, serverA), newTaggingClient(t, serverB)
	releaseA, releasedA := countRelease()
	releaseB, releasedB := countRelease()
	stream := NewSwitchableStream(context.Background(), clientA, releaseA, "session", false)
	defer stream.Release()

	processTag(t, stream, 1)
	tags, err := switchWhileStreaming(t, context.Background(), stream, clientB, releaseB)
	if err == nil || !strings.Contains(err.Error(), "INFERENCE_FAILED") {
		t.Fatalf("SwitchTo() error = %v, want warm-up failure", err)
	}
	if tag := processTag(t, stream, 5000); tag != "a" || stream.Target() != "a" {
		t.Fatalf("frame after failed switch from %q (target %q), want a", tag, stream.Target())
	}
	for _, tag := range tags {
		if tag != "a" {
			t.Fatalf("frames during failed switch = %v, want only a", tags)
		}
	}
	if releasedA.Load() != 0 || releasedB.Load() != 1 {
		t.Fatalf("releases a=%d b=%d, want b released once", releasedA.Load(), releasedB.Load())
	}
}

// 프레임이 흐르지 않는 스트림(일시 정지 등)은 예열 없이 바로 바꾼다.
func TestSwitchableStreamSwitchesIdleStreamImmediately(t *testing.T) {
	serverA, serverB := &taggingAIServer{name: "a"}, &taggingAIServer{name: "b"}
	releaseA, releasedA := countRelease()
	releaseB, _ := countRelease()
	stream := NewSwitchableStream(context.Background(), newTaggingClient(t, serverA), releaseA, "session", false)
	defer stream.Release()

	if err := stream.SwitchTo(context.Background(), newTaggingClient(t, serverB), releaseB); err != nil {
		t.Fatalf("SwitchTo() error = %v", err)
	}
	if tag := processTag(t, stream, 1); tag != "b" {
		t.Fatalf("first frame after idle switch from %q, want b", tag)
	}
	if serverA.frames.Load() != 0 || releasedA.Load() != 1 {
		t.Fatalf("worker a frames=%d releases=%d, want untouched and released", serverA.frames.Load(), releasedA.Load())
	}
}

// 예열이 끝나기 전에 ctx가 끝나면 지금 워커를 그대로 두고 후보를 놓는다.
func TestSwitchableStreamAbandonsSwitchOnContextEnd(t *testing.T) {
	serverA, serverB := &taggingAIServer{name: "a"}, &taggingAIServer{name: "b", block: make(chan struct{})}
	defer close(serverB.block)
	clientA, clientB := newTaggingClient(t, serverA), newTaggingClient(t, serverB)
	releaseA, _ := countRelease()
	releaseB, releasedB := countRelease()
	stream := NewSwitchableStream(context.Background(), clientA, releaseA, "session", false)
	defer stream.Release()

	processTag(t, stream, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := switchWhileStreaming(t, ctx, stream, clientB, releaseB)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("SwitchTo() error = %v, want deadline exceeded", err)
	}
	if tag := processTag(t, stream, 5000); tag != "a" {
		t.Fatalf("frame after abandoned switch from %q, want a", tag)
	}
	deadline := time.Now().Add(2 * time.Second)
	for releasedB.Load() != 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if releasedB.Load() != 1 {
		t.Fatalf("abandoned candidate released %d times, want once", releasedB.Load())
	}
}

// 예열 중 AI 입력이 멈추거나 파이프라인이 끝나면 전환을 그만둔다.
func TestSwitchableStreamStopsSwitchOnCloseAndRelease(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(*SwitchableStream)
		want error
	}{
		{"close", (*SwitchableStream).Close, ErrSwitchInterrupted},
		{"release", (*SwitchableStream).Release, ErrSwitchClosed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			serverA, serverB := &taggingAIServer{name: "a"}, &taggingAIServer{name: "b", block: make(chan struct{})}
			defer close(serverB.block)
			releaseA, _ := countRelease()
			releaseB, _ := countRelease()
			stream := NewSwitchableStream(context.Background(), newTaggingClient(t, serverA), releaseA, "session", false)
			defer stream.Release()
			processTag(t, stream, 1)

			result := make(chan error, 1)
			go func() { result <- stream.SwitchTo(context.Background(), newTaggingClient(t, serverB), releaseB) }()
			deadline := time.Now().Add(2 * time.Second)
			for serverB.frames.Load() == 0 && time.Now().Before(deadline) {
				processTag(t, stream, 2)
				time.Sleep(5 * time.Millisecond)
			}
			tc.stop(stream)
			select {
			case err := <-result:
				if !errors.Is(err, tc.want) {
					t.Fatalf("SwitchTo() error = %v, want %v", err, tc.want)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("SwitchTo() did not return after the stream stopped")
			}
		})
	}
}

// 이미 넘어가는 중이면 두 번째 전환을 거절하고 그 배정을 바로 돌려준다.
func TestSwitchableStreamRejectsConcurrentSwitch(t *testing.T) {
	serverA, serverB, serverC := &taggingAIServer{name: "a"}, &taggingAIServer{name: "b", block: make(chan struct{})}, &taggingAIServer{name: "c"}
	defer close(serverB.block)
	releaseA, _ := countRelease()
	releaseB, _ := countRelease()
	releaseC, releasedC := countRelease()
	clientA := newTaggingClient(t, serverA)
	stream := NewSwitchableStream(context.Background(), clientA, releaseA, "session", false)
	defer stream.Release()
	processTag(t, stream, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go stream.SwitchTo(ctx, newTaggingClient(t, serverB), releaseB)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		stream.mu.Lock()
		pending := stream.candidate != nil
		stream.mu.Unlock()
		if pending {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := stream.SwitchTo(context.Background(), newTaggingClient(t, serverC), releaseC); !errors.Is(err, ErrSwitchInProgress) {
		t.Fatalf("second SwitchTo() error = %v, want ErrSwitchInProgress", err)
	}
	if releasedC.Load() != 1 {
		t.Fatalf("rejected switch released %d times, want once", releasedC.Load())
	}
	if err := stream.SwitchTo(context.Background(), clientA, func() {}); err == nil {
		t.Fatal("SwitchTo() onto the current worker succeeded")
	}
}
