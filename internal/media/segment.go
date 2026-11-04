package media

import (
	"bufio"
	"context"
	"errors"
	"sync/atomic"

	"inno-live-server/internal/config"
)

// segment는 FFmpeg 프로세스 하나로 도는 디코드·인코드 구간의 공통 골격이다(P2-1).
// 쓰기 고루틴이 입력을 stdin에 쓰고 쓴 순서대로 메타데이터 큐에 넣으면, 읽기
// 루프가 큐에서 하나 꺼낼 때마다 stdout에서 결과 하나를 읽어 채운다. 구간마다
// 다른 부분만 아래 함수로 받는다.
type segment struct {
	process *ffmpegProcess
	queue   int
	// stalled가 있으면 디코더 구간이다. 큐가 찬 채로 정지 기한이 지나면 디코더가
	// 멈춘 것으로 보고 구간을 끝낸다(P0-4). 없으면 큐에 자리가 날 때까지 기다린다.
	stalled *atomic.Bool
	// accept는 첫 프레임 다음 입력을 거른다. keep이 false면 버리고, stop이 true면
	// 그 프레임을 쓰지 않고 구간을 끝낸다. 쓰기 고루틴에서만 불린다. nil이면 모두 받는다.
	accept func(item frame) (out frame, keep, stop bool)
	// write는 프레임을 stdin에 쓰고 큐에 넣을 프레임을 돌려준다.
	write func(item frame) (frame, error)
	// read는 큐에서 꺼낸 프레임의 결과를 stdout에서 읽어 채운다. 오류에는 문맥을 붙인다.
	read func(item frame) (frame, error)
	// writeFailed는 쓰기 쪽 오류에 문맥을 붙인다.
	writeFailed func(error) error
}

// runSegment는 입력이 끝나거나 accept가 구간을 끝낼 때까지 돌고, 이미 쓴 프레임을 모두
// 읽어낸 뒤 돌아온다. accept가 구간을 끝내며 남긴 값은 runSegment가 돌아온 뒤에 읽는다.
func (t *FFmpegTranscoder) runSegment(ctx context.Context, s segment, first frame, input <-chan frame, output chan<- frame) error {
	metadata := make(chan frame, s.queue)
	writeError := make(chan error, 1)
	go func() {
		defer close(metadata)
		defer s.process.stdin.Close()
		writeError <- t.feedSegment(ctx, s, first, input, metadata)
	}()

	for item := range metadata {
		item, err := s.read(item)
		if err != nil {
			return err
		}
		select {
		case output <- item:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := <-writeError; err != nil && !errors.Is(err, context.Canceled) {
		if errors.Is(err, errDecoderStalled) {
			return errDecoderStalled
		}
		return s.writeFailed(err)
	}
	return nil
}

// feedSegment는 runSegment의 쓰기 쪽이다. 첫 프레임은 accept를 거치지 않는다.
func (t *FFmpegTranscoder) feedSegment(ctx context.Context, s segment, first frame, input <-chan frame, metadata chan<- frame) error {
	put := func(item frame) error {
		item, err := s.write(item)
		if err != nil {
			return err
		}
		if s.stalled != nil {
			return t.sendDecodeMetadata(ctx, metadata, item, s.stalled)
		}
		select {
		case metadata <- item:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := put(first); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case item, ok := <-input:
			if !ok {
				return nil
			}
			if s.accept != nil {
				var keep, stop bool
				if item, keep, stop = s.accept(item); stop {
					return nil
				} else if !keep {
					continue
				}
			}
			if err := put(item); err != nil {
				return err
			}
		}
	}
}

// readDecodedFrame은 디코더 출력 한 장을 전송 형식대로 읽는다.
func (t *FFmpegTranscoder) readDecodedFrame(reader *bufio.Reader, rawSize int) ([]byte, error) {
	if t.options.WireFormat == config.WireFormatRaw {
		return readRawFrame(reader, rawSize)
	}
	return readJPEG(reader)
}
