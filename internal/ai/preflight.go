package ai

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"strings"
	"time"

	aiv1 "inno-live-server/api/gen/aiv1"
)

// preflightDim은 합성 프로브 프레임의 기본 한 변 길이다. 내용은 상관없지만
// 크기는 AI 서버가 받아들이는 값이어야 한다 — innolive-ai는 들어온 프레임을
// B1-640 파이프라인 기준으로 검증하고 너무 작은 이미지는 DECODE_FAILED로
// 거부하므로, 프로브도 640으로 맞춘다.
//
// 디코더 핀이 켜져 있으면 프로브는 그 대신 핀 장변만큼 커진다 —
// probeDimension을 보라. 핀이 AI의 MAX_LONG_EDGE보다 크면 AI가 모든 세션의
// 모든 프레임을 거부하고, fail-closed가 그것을 영구 블랙아웃으로 바꾼다.
// 640으로만 프로브하면 그 조합이 조용히 부팅된다(#122).
const preflightDim = 640

// probeDimension은 프로브 한 변의 길이를 고른다. 핀 장변짜리 정사각형은
// 파이프라인이 만들어낼 수 있는 최악의 경우이므로(실제 프레임은 한 변만 그
// 길이다), 이 값으로 보내면 장변 제한과 픽셀 수 제한을 동시에 최대로 시험한다.
func probeDimension(pinLongEdge int) int {
	if pinLongEdge > 0 {
		return pinLongEdge
	}
	return preflightDim
}

// Preflight는 사용자 세션이 하나라도 돌기 전에 모든 AI worker를 상대로
// ProcessVideo 왕복 전체를 검증한다. 각 target에 wireFormat에 맞는 합성 프레임을
// 하나씩 보내고, worker가 timestamp를 되돌려주고 status_message=="success"(대소문자
// 무시)를 반환하며 비어 있지 않은 data를 보낼 것을 요구한다 — Processor.ProcessImage가
// 프레임마다 강제하는 것과 같은 계약이다. 이로써 조용하고 영구적인 세션별 블랙아웃
// (예: raw 기본값에 JPEG만 받는 PIL/OpenCV AI 서버가 물린 경우)이 시끄러운 부팅
// 실패로 바뀐다. 겸해서 실제 모델의 첫 추론 JIT 비용을 미리 치르는 warmup 호출
// 역할도 한다. target별 deadline은 timeout에서 가져오며, 첫 추론 warmup을 흡수할 수
// 있도록 정상 상태의 AI_GRPC_TIMEOUT과 의도적으로 분리했다.
func (p *Pool) Preflight(ctx context.Context, wireFormat string, timeout time.Duration, pinLongEdge int) error {
	var errs []error
	for _, client := range p.clients {
		callCtx, cancel := context.WithTimeout(ctx, timeout)
		err := client.Preflight(callCtx, wireFormat, pinLongEdge)
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", client.Address(), err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("AI preflight failed on %d/%d target(s): %w", len(errs), len(p.clients), errors.Join(errs...))
	}
	return nil
}

// Preflight는 이 worker를 상대로 합성 ProcessVideo 왕복을 한 번 수행한다.
// deadline은 호출자가 ctx로 정한다.
func (c *Client) Preflight(ctx context.Context, wireFormat string, pinLongEdge int) error {
	if wireFormat == "raw" {
		return errors.New("AI_FRAME_WIRE_FORMAT=raw is not supported by this AI server's proto (no width/height/pix_fmt fields) — use jpeg")
	}
	data, err := syntheticFrame(probeDimension(pinLongEdge))
	if err != nil {
		return err
	}
	stream, err := c.client.ProcessVideo(ctx)
	if err != nil {
		return fmt.Errorf("open preflight stream: %w", err)
	}
	const timestamp int64 = 1
	request := &aiv1.VideoChunk{
		Data:      data,
		Timestamp: timestamp,
		SessionId: "preflight",
	}
	if err := stream.Send(request); err != nil {
		return fmt.Errorf("send preflight frame: %w", err)
	}
	response, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("receive preflight response: %w", err)
	}
	_ = stream.CloseSend()
	if response.GetTimestamp() != timestamp {
		return fmt.Errorf("preflight timestamp not echoed: sent=%d received=%d (the AI server must echo the request timestamp unmodified)", timestamp, response.GetTimestamp())
	}
	if response.GetErrorCode() != "" {
		return fmt.Errorf("preflight failed: error_code=%s error_message=%q", response.GetErrorCode(), response.GetErrorMessage())
	}
	if !strings.EqualFold(response.GetStatusMessage(), "success") {
		return fmt.Errorf("preflight status=%q, want \"success\"", response.GetStatusMessage())
	}
	if len(response.GetData()) == 0 {
		return errors.New("preflight response returned an empty frame")
	}
	return nil
}

// syntheticFrame은 최소한의 회색조 JPEG 프로브 프레임을 만든다. jpeg만
// 지원한다 — 이 AI 서버의 proto에는 width/height/pix_fmt 필드가 없어서 raw
// yuv420p 와이어 포맷은 전달 자체가 불가능하다(Client.Preflight가 이 함수를
// 부르기 전에 wireFormat=="raw"를 거부한다).
func syntheticFrame(dim int) ([]byte, error) {
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, image.NewGray(image.Rect(0, 0, dim, dim)), &jpeg.Options{Quality: 75}); err != nil {
		return nil, fmt.Errorf("encode preflight jpeg: %w", err)
	}
	return encoded.Bytes(), nil
}
