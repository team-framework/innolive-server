package ai

import (
	"context"
	"testing"
	"time"

	aiv1 "inno-live-server/api/gen/aiv1"

	"google.golang.org/grpc"
)

// slowAIServer는 첫 추론(TensorRT 예열)처럼 응답이 느린 워커다.
type slowAIServer struct {
	aiv1.UnimplementedAiProcessorServer
	delay time.Duration
}

func (s slowAIServer) ProcessVideo(stream grpc.BidiStreamingServer[aiv1.VideoChunk, aiv1.ProcessedVideoChunk]) error {
	request, err := stream.Recv()
	if err != nil {
		return nil
	}
	time.Sleep(s.delay)
	return stream.Send(&aiv1.ProcessedVideoChunk{Timestamp: request.Timestamp, StatusMessage: "success", Data: []byte{1}})
}

// 기동 preflight가 워커를 하나씩 기다리면 워커 수만큼 기동이 늦어지고, 배포에서는
// 새 슬롯 헬스 대기가 그만큼 길어진다(P1-27). 워커는 서로 독립이라 함께 확인한다.
func TestPoolPreflightChecksTargetsConcurrently(t *testing.T) {
	const delay = 300 * time.Millisecond
	pool := &Pool{clients: []*Client{
		preflightClient(t, slowAIServer{delay: delay}),
		preflightClient(t, slowAIServer{delay: delay}),
		preflightClient(t, slowAIServer{delay: delay}),
		preflightClient(t, slowAIServer{delay: delay}),
	}}
	started := time.Now()
	if err := pool.Preflight(context.Background(), "jpeg", 5*time.Second, 0); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed >= 2*delay {
		t.Fatalf("preflight of 4 workers took %s, want them checked concurrently", elapsed)
	}
}
