package ai

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	aiv1 "inno-live-server/api/gen/aiv1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"
)

// watchedTestClient는 New와 같은 인터셉터를 단 클라이언트를 bufconn 서버에 붙이고,
// 받은 고장 신호 수를 센다.
func watchedTestClient(t *testing.T, server aiv1.AiProcessorServer) (*Client, *atomic.Int32) {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	aiv1.RegisterAiProcessorServer(grpcServer, server)
	go grpcServer.Serve(listener)
	t.Cleanup(grpcServer.Stop)

	client, err := newClient("passthrough:///bufnet", time.Second,
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	var suspects atomic.Int32
	client.onSuspect = func(*Client) { suspects.Add(1) }
	return client, &suspects
}

// errorCodeServer는 모든 프레임에 같은 오류 코드로 답한다.
type errorCodeServer struct {
	aiv1.UnimplementedAiProcessorServer
	code string
}

func (s errorCodeServer) ProcessVideo(stream grpc.BidiStreamingServer[aiv1.VideoChunk, aiv1.ProcessedVideoChunk]) error {
	for {
		request, err := stream.Recv()
		if err != nil {
			return nil
		}
		if err := stream.Send(&aiv1.ProcessedVideoChunk{Timestamp: request.Timestamp, ErrorCode: s.code, ErrorMessage: "test"}); err != nil {
			return err
		}
	}
}

// 런타임이 응답 불가인 워커는 ProcessVideo를 UNAVAILABLE로 거절한다. 2026-10-20
// 프로덕션에서 이 상태의 워커가 세션을 계속 배정받았다(#499).
func TestStreamReportsUnavailableWorker(t *testing.T) {
	client, suspects := watchedTestClient(t, unavailableAIServer{})
	stream := client.NewStream(context.Background(), "session", false)
	defer stream.Close()

	if _, err := stream.Process([]byte("frame"), 1, 0, 0, "", nil); err == nil {
		t.Fatal("Process() error = nil, want worker failure")
	}
	if suspects.Load() == 0 {
		t.Fatal("unavailable worker was not reported")
	}
}

// INFERENCE_TIMEOUT 응답을 낸 innolive-ai 워커는 스스로 응답 불가로 돌아선다.
func TestStreamReportsWorkerFailureCode(t *testing.T) {
	client, suspects := watchedTestClient(t, errorCodeServer{code: "INFERENCE_TIMEOUT"})
	stream := client.NewStream(context.Background(), "session", false)
	defer stream.Close()

	response, err := stream.Process([]byte("frame"), 1, 0, 0, "", nil)
	if err != nil {
		t.Fatalf("Process() error = %v, want the error response itself", err)
	}
	if response.GetErrorCode() != "INFERENCE_TIMEOUT" {
		t.Fatalf("error_code = %q, want INFERENCE_TIMEOUT", response.GetErrorCode())
	}
	if suspects.Load() != 1 {
		t.Fatalf("suspect reports = %d, want 1", suspects.Load())
	}
}

func TestStreamDoesNotReportHealthyWorkerOrFrameErrors(t *testing.T) {
	for name, server := range map[string]aiv1.AiProcessorServer{
		"echo":          echoAIServer{},
		"decode failed": errorCodeServer{code: "DECODE_FAILED"},
	} {
		t.Run(name, func(t *testing.T) {
			client, suspects := watchedTestClient(t, server)
			stream := client.NewStream(context.Background(), "session", false)
			for timestamp := range int64(3) {
				if _, err := stream.Process([]byte("frame"), timestamp, 0, 0, "", nil); err != nil {
					t.Fatalf("Process() error = %v", err)
				}
			}
			stream.Close()
			if suspects.Load() != 0 {
				t.Fatalf("suspect reports = %d, want 0", suspects.Load())
			}
		})
	}
}

// 파이프라인을 내리며 서버가 스스로 끊은 스트림은 워커 고장이 아니다.
func TestStreamDoesNotReportCanceledSession(t *testing.T) {
	client, suspects := watchedTestClient(t, unavailableAIServer{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stream := client.NewStream(ctx, "session", false)
	defer stream.Close()

	_, _ = stream.Process([]byte("frame"), 1, 0, 0, "", nil)
	if suspects.Load() != 0 {
		t.Fatalf("suspect reports = %d, want 0 for a canceled session", suspects.Load())
	}
}

// 점검 프로브의 실패는 프로브가 직접 판정한다. 신호로 알리면 실패한 점검이 다시
// 점검을 부른다.
func TestPreflightDoesNotReportSuspect(t *testing.T) {
	client, suspects := watchedTestClient(t, unavailableAIServer{})
	if err := client.Preflight(context.Background(), "jpeg", 0); err == nil {
		t.Fatal("Preflight() error = nil, want worker failure")
	}
	if suspects.Load() != 0 {
		t.Fatalf("suspect reports = %d, want 0 for a probe", suspects.Load())
	}
}

// 오류 없이 돌아온 세션 프레임만 "막 처리함"으로 센다. 상시 점검은 이것을 보고
// 송출 중인 워커를 건너뛴다.
func TestStreamRecordsServedFramesOnly(t *testing.T) {
	for name, tc := range map[string]struct {
		server aiv1.AiProcessorServer
		served bool
	}{
		"echo":          {echoAIServer{}, true},
		"decode failed": {errorCodeServer{code: "DECODE_FAILED"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			client, _ := watchedTestClient(t, tc.server)
			stream := client.NewStream(context.Background(), "session", false)
			defer stream.Close()
			if _, err := stream.Process([]byte("frame"), 1, 0, 0, "", nil); err != nil {
				t.Fatalf("Process() error = %v", err)
			}
			if got := client.servedFrameWithin(time.Minute); got != tc.served {
				t.Fatalf("servedFrameWithin() = %t, want %t", got, tc.served)
			}
		})
	}
}

// 점검 프레임은 "막 처리함"으로 세지 않는다.
func TestPreflightDoesNotRecordServedFrame(t *testing.T) {
	client, _ := watchedTestClient(t, echoAIServer{})
	if err := client.Preflight(context.Background(), "jpeg", 0); err != nil {
		t.Fatalf("Preflight() error = %v", err)
	}
	if client.servedFrameWithin(time.Minute) {
		t.Fatal("a probe frame counted as a served session frame")
	}
}
