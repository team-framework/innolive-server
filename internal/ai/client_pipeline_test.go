package ai

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	aiv1 "inno-live-server/api/gen/aiv1"

	"google.golang.org/grpc"
)

// aheadAIServer는 요청을 batch개 받은 뒤에야 응답을 보낸다. 앞 응답을 기다리고
// 다음 요청을 보내는 클라이언트는 여기서 멈추므로, 미리 보내기를 확인할 수 있다(#495).
type aheadAIServer struct {
	aiv1.UnimplementedAiProcessorServer
	batch int
}

func (s aheadAIServer) ProcessVideo(stream grpc.BidiStreamingServer[aiv1.VideoChunk, aiv1.ProcessedVideoChunk]) error {
	for {
		requests := make([]*aiv1.VideoChunk, 0, s.batch)
		for len(requests) < s.batch {
			request, err := stream.Recv()
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
			requests = append(requests, request)
		}
		for _, request := range requests {
			if err := stream.Send(&aiv1.ProcessedVideoChunk{Data: request.Data, Timestamp: request.Timestamp, StatusMessage: "success"}); err != nil {
				return err
			}
		}
	}
}

// stallingAIServer는 첫 ProcessVideo 호출에 응답하지 않고, 그 뒤 호출에는 바로 응답한다.
type stallingAIServer struct {
	aiv1.UnimplementedAiProcessorServer
	calls *atomic.Int32
}

func (s stallingAIServer) ProcessVideo(stream grpc.BidiStreamingServer[aiv1.VideoChunk, aiv1.ProcessedVideoChunk]) error {
	if s.calls.Add(1) == 1 {
		<-stream.Context().Done()
		return stream.Context().Err()
	}
	return echoAIServer{}.ProcessVideo(stream)
}

func TestSubmitSendsAheadAndMatchesResponsesInOrder(t *testing.T) {
	client := newStreamTestClient(t, aheadAIServer{batch: 2})
	stream := client.NewStream(context.Background(), "", false)
	defer stream.Close()

	for round := int64(0); round < 3; round++ {
		first, err := stream.Submit([]byte("a"), 10*round+1, 0, 0, "", nil)
		if err != nil {
			t.Fatalf("Submit(first) error = %v", err)
		}
		second, err := stream.Submit([]byte("b"), 10*round+2, 0, 0, "", nil)
		if err != nil {
			t.Fatalf("Submit(second) error = %v", err)
		}
		for index, frame := range []*PendingFrame{first, second} {
			response, err := frame.Wait()
			if err != nil {
				t.Fatalf("round %d frame %d Wait() error = %v", round, index, err)
			}
			if want := 10*round + int64(index) + 1; response.GetTimestamp() != want {
				t.Fatalf("round %d frame %d timestamp = %d, want %d", round, index, response.GetTimestamp(), want)
			}
		}
	}
}

func TestWaitTimeoutFailsEveryPendingFrameAndReopensTheStream(t *testing.T) {
	var calls atomic.Int32
	client := newStreamTestClient(t, stallingAIServer{calls: &calls})
	client.timeout = 100 * time.Millisecond
	stream := client.NewStream(context.Background(), "", false)
	defer stream.Close()

	first, err := stream.Submit([]byte("a"), 1, 0, 0, "", nil)
	if err != nil {
		t.Fatalf("Submit(first) error = %v", err)
	}
	second, err := stream.Submit([]byte("b"), 2, 0, 0, "", nil)
	if err != nil {
		t.Fatalf("Submit(second) error = %v", err)
	}
	if _, err := first.Wait(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first Wait() error = %v, want deadline exceeded", err)
	}
	started := time.Now()
	if _, err := second.Wait(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second Wait() error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 50*time.Millisecond {
		t.Fatalf("second Wait() took %v after the stream was abandoned", elapsed)
	}

	wait, err := stream.SubmitFrame([]byte("c"), 3, 0, 0, "", nil)
	if err != nil {
		t.Fatalf("SubmitFrame() after timeout error = %v", err)
	}
	response, err := wait()
	if err != nil || response.GetTimestamp() != 3 {
		t.Fatalf("frame on the reopened stream = %+v, %v", response, err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("ProcessVideo calls = %d, want 2", got)
	}
}

func TestCloseFailsPendingFrames(t *testing.T) {
	var calls atomic.Int32
	client := newStreamTestClient(t, stallingAIServer{calls: &calls})
	stream := client.NewStream(context.Background(), "", false)

	frame, err := stream.Submit([]byte("a"), 1, 0, 0, "", nil)
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	stream.Close()
	if _, err := frame.Wait(); !errors.Is(err, errStreamClosed) {
		t.Fatalf("Wait() after Close error = %v, want stream closed", err)
	}
}

func TestPendingFrameReportsContextCanceledWhenSessionCanceled(t *testing.T) {
	var calls atomic.Int32
	client := newStreamTestClient(t, stallingAIServer{calls: &calls})
	ctx, cancel := context.WithCancel(context.Background())
	stream := client.NewStream(ctx, "", false)
	defer stream.Close()

	frame, err := stream.Submit([]byte("a"), 1, 0, 0, "", nil)
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	cancel()
	if _, err := frame.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait() after session cancel error = %v, want context canceled", err)
	}
}
