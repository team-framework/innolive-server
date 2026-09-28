package ai

import (
	"context"
	"testing"
	"time"

	aiv1 "inno-live-server/api/gen/aiv1"

	"google.golang.org/grpc"
)

type idleVideoStream struct {
	grpc.ClientStream
}

func (idleVideoStream) Send(*aiv1.VideoChunk) error { return nil }
func (idleVideoStream) Recv() (*aiv1.ProcessedVideoChunk, error) {
	return &aiv1.ProcessedVideoChunk{}, nil
}
func (idleVideoStream) CloseSend() error { return nil }

type idleProcessorClient struct {
	aiv1.AiProcessorClient
}

func (idleProcessorClient) ProcessVideo(context.Context, ...grpc.CallOption) (aiv1.AiProcessor_ProcessVideoClient, error) {
	return idleVideoStream{}, nil
}

// 세션 ctx가 취소되면 Process는 전송 고루틴을 기다리지 않고 돌아와 스트림을
// 비운다. 늦게 실행된 고루틴이 비워진 스트림을 참조하면 서버 전체가 죽는다(#316).
func TestProcessAfterCancelDoesNotUseResetStream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := 0; i < 5000; i++ {
		stream := &Stream{ctx: ctx, client: idleProcessorClient{}, timeout: time.Second}
		if _, err := stream.Process([]byte("frame"), 0, 0, 0, ""); err == nil {
			// 고루틴이 먼저 끝나면 성공할 수도 있다. 오류 여부는 이 테스트의 관심사가 아니다.
			continue
		}
	}
	// 늦게 실행되는 고루틴이 panic을 낼 시간을 준다.
	time.Sleep(100 * time.Millisecond)
}
