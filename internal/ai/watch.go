package ai

import (
	"context"
	"errors"
	"io"

	aiv1 "inno-live-server/api/gen/aiv1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// workerFailureCodes는 innolive-ai가 런타임을 응답 불가로 돌리며 내는 프레임 오류
// 코드다(ai_processor_server.py _run_inference). 이 응답을 낸 워커는 재시작 전까지
// 새 ProcessVideo를 UNAVAILABLE로 거절한다 — 2026-10-20 프로덕션에서 워커 2대가
// INFERENCE_TIMEOUT 한 번 뒤 그 상태로 남았다(#499).
var workerFailureCodes = map[string]bool{
	"INFERENCE_TIMEOUT": true,
	"INFERENCE_FAILED":  true,
	"PROCESSING_FAILED": true,
}

// probeContextKey가 붙은 ctx의 스트림은 상태 점검 자신이 연 것이라 고장 신호로
// 알리지 않는다. 알리면 실패한 점검이 다시 점검을 부른다.
type probeContextKey struct{}

func withProbe(ctx context.Context) context.Context {
	return context.WithValue(ctx, probeContextKey{}, true)
}

// watchStream은 ProcessVideo 스트림을 감싸 워커 고장 신호를 onSuspect로 알린다.
// 신호는 판정이 아니다 — 받은 쪽(상태 점검)이 그 워커를 실제로 다시 점검해 준비
// 상태를 정한다. 세션 ctx 취소처럼 서버가 스스로 끊은 경우는 신호가 아니다.
func (c *Client) watchStream(
	ctx context.Context,
	desc *grpc.StreamDesc,
	conn *grpc.ClientConn,
	method string,
	streamer grpc.Streamer,
	options ...grpc.CallOption,
) (grpc.ClientStream, error) {
	stream, err := streamer(ctx, desc, conn, method, options...)
	if method != aiv1.AiProcessor_ProcessVideo_FullMethodName || ctx.Value(probeContextKey{}) != nil {
		return stream, err
	}
	if err != nil {
		c.reportIfWorkerFailure(ctx, err)
		return nil, err
	}
	return &watchedStream{ClientStream: stream, client: c, ctx: ctx}, nil
}

type watchedStream struct {
	grpc.ClientStream
	client *Client
	ctx    context.Context
}

// SendMsg가 실패하면 워커가 스트림을 먼저 끝낸 것이다. 실제 사유(UNAVAILABLE 등)는
// Recv에서만 보이는데 호출자는 Recv 없이 스트림을 버리므로 여기서도 알린다.
func (s *watchedStream) SendMsg(message any) error {
	err := s.ClientStream.SendMsg(message)
	if err != nil {
		s.client.reportIfWorkerFailure(s.ctx, err)
	}
	return err
}

func (s *watchedStream) RecvMsg(message any) error {
	err := s.ClientStream.RecvMsg(message)
	if err != nil {
		s.client.reportIfWorkerFailure(s.ctx, err)
		return err
	}
	if chunk, ok := message.(*aiv1.ProcessedVideoChunk); ok && workerFailureCodes[chunk.GetErrorCode()] {
		s.client.reportSuspect()
	}
	return nil
}

// reportIfWorkerFailure는 워커가 연결을 받지 않거나(UNAVAILABLE) 스트림을 먼저
// 끝낸(io.EOF) 경우를 알린다. 서버가 스스로 스트림을 끊었으면(ctx 취소) 알리지
// 않고, 타임아웃은 서버 쪽 사정일 수 있어 알리지 않는다.
func (c *Client) reportIfWorkerFailure(ctx context.Context, err error) {
	if ctx.Err() != nil {
		return
	}
	if errors.Is(err, io.EOF) || status.Code(err) == codes.Unavailable {
		c.reportSuspect()
	}
}

func (c *Client) reportSuspect() {
	if c.onSuspect != nil {
		c.onSuspect(c)
	}
}
