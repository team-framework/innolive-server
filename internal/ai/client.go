package ai

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	aiv1 "inno-live-server/api/gen/aiv1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
)

// maxAIRecvMsgSize는 AI 계약의 수신 하한(5 MiB 이상)을 최악의 처리 프레임에 여유를
// 두고 맞춘다.
const maxAIRecvMsgSize = 8 << 20

type Client struct {
	address string
	conn    *grpc.ClientConn
	client  aiv1.AiProcessorClient
	timeout time.Duration
}

func New(address string, timeout time.Duration) (*Client, error) {
	if address == "" {
		return nil, errors.New("AI gRPC address is empty")
	}
	conn, err := grpc.NewClient(address,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		// 최악의 ProcessedVideoChunk(모자이크 JPEG + 얼굴별 메타데이터)는 gRPC 기본 수신
		// 한도 4 MiB를 넘을 수 있다. AI 계약은 5 MiB 이상을 요구한다.
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxAIRecvMsgSize)),
	)
	if err != nil {
		return nil, fmt.Errorf("create AI gRPC client: %w", err)
	}
	return &Client{
		address: address,
		conn:    conn,
		client:  aiv1.NewAiProcessorClient(conn),
		timeout: timeout,
	}, nil
}

func (c *Client) Close() error { return c.conn.Close() }

func (c *Client) State() string { return c.conn.GetState().String() }

func (c *Client) Ready() bool { return c.conn.GetState() == connectivity.Ready }

func (c *Client) Address() string { return c.address }

// NewStream은 세션 하나의 ProcessVideo 흐름을 만든다. faceSwap이면 얼굴을 blur
// 대신 프리셋 얼굴로 합성하도록 요청한다. 합성 모드는 AI 계약상 한 RPC 동안
// 고정이라 스트림 수명 내내 매 프레임 같은 값을 싣는다.
func (c *Client) NewStream(ctx context.Context, sessionID string, faceSwap bool) *Stream {
	anonymization := aiv1.FaceAnonymizationMode_FACE_ANONYMIZATION_MODE_BLUR
	if faceSwap {
		anonymization = aiv1.FaceAnonymizationMode_FACE_ANONYMIZATION_MODE_FACE_SWAP
	}
	return &Stream{ctx: ctx, client: c.client, timeout: c.timeout, sessionID: sessionID, anonymization: anonymization}
}

// whitelistTimeout은 프레임 스트림 타임아웃보다 훨씬 길다. 첫 AddWhitelist가 얼굴
// 인식 모델을 JIT 컴파일할 수 있고(Blackwell에서 수십 초), 화이트리스트 작업은
// 드문 사용자 동작이라 핫 패스가 아니다.
func (c *Client) whitelistTimeout() time.Duration {
	if c.timeout < 120*time.Second {
		return 120 * time.Second
	}
	return c.timeout
}

func (c *Client) AddWhitelist(ctx context.Context, sessionID string, data []byte) (*aiv1.WhitelistResponse, error) {
	callCtx, cancel := context.WithTimeout(ctx, c.whitelistTimeout())
	defer cancel()
	response, err := c.client.AddWhitelist(callCtx, &aiv1.FaceData{Data: data, SessionId: sessionID})
	if err != nil {
		return nil, fmt.Errorf("call AI AddWhitelist: %w", err)
	}
	return response, nil
}

func (c *Client) DeleteWhitelist(ctx context.Context, sessionID, entryID string) (*aiv1.WhitelistResponse, error) {
	callCtx, cancel := context.WithTimeout(ctx, c.whitelistTimeout())
	defer cancel()
	response, err := c.client.DeleteWhitelist(callCtx, &aiv1.DeleteWhitelistRequest{SessionId: sessionID, EntryId: entryID})
	if err != nil {
		return nil, fmt.Errorf("call AI DeleteWhitelist: %w", err)
	}
	return response, nil
}

func (c *Client) GetWhitelistStatus(ctx context.Context, sessionID string) (*aiv1.GetWhitelistStatusResponse, error) {
	callCtx, cancel := context.WithTimeout(ctx, c.whitelistTimeout())
	defer cancel()
	response, err := c.client.GetWhitelistStatus(callCtx, &aiv1.GetWhitelistStatusRequest{SessionId: sessionID})
	if err != nil {
		return nil, fmt.Errorf("call AI GetWhitelistStatus: %w", err)
	}
	return response, nil
}

type Stream struct {
	ctx       context.Context
	client    aiv1.AiProcessorClient
	timeout   time.Duration
	sessionID string
	// anonymization은 얼굴 처리 방식이다. 0(blur)이면 종전 요청과 같다.
	anonymization aiv1.FaceAnonymizationMode

	mu           sync.Mutex
	stream       aiv1.AiProcessor_ProcessVideoClient
	streamCancel context.CancelFunc
	// generation은 reset마다 올라간다. 미리 보낸 요청(Submit)의 응답 수신 고루틴이
	// 끊긴 이전 스트림의 응답을 새 스트림의 요청에 짝짓지 않게 한다(#495).
	generation uint64
	// receiving은 응답 수신 고루틴을 띄운 스트림이다. pending은 보냈지만 응답을 아직
	// 받지 않은 요청을 보낸 순서대로 담는다. Submit을 쓸 때만 채워진다.
	receiving aiv1.AiProcessor_ProcessVideoClient
	pending   []*PendingFrame
}

// PendingFrame은 Submit으로 보낸 요청 하나다. Wait가 응답을 기다린다.
type PendingFrame struct {
	stream     *Stream
	generation uint64
	sentAt     time.Time
	result     chan pendingResult
}

type pendingResult struct {
	response *aiv1.ProcessedVideoChunk
	err      error
}

// Process는 한 프레임을 AI로 보내고 응답을 받는다. pixFmt가 비어 있지 않으면
// data는 raw 픽셀(예: "yuv420p")이며, AI가 해석할 수 있도록 width/height를 함께
// 싣는다. pixFmt가 비면 data는 자기서술적 JPEG이라 width/height는 무시된다.
// mosaic은 이 프레임의 블러 설정이며 nil이면 AI 기본값이다.
func (s *Stream) Process(data []byte, timestamp int64, width, height uint16, pixFmt string, mosaic *aiv1.MosaicConfig) (*aiv1.ProcessedVideoChunk, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.ensureStream(); err != nil {
		return nil, err
	}

	callCtx, cancel := context.WithTimeout(s.ctx, s.timeout)
	defer cancel()
	request := s.request(data, timestamp, width, height, pixFmt, mosaic)
	// 고루틴은 ctx가 끝나면 기다리지 않으므로 reset 뒤에 실행될 수 있다.
	// s.stream을 실행 시점에 읽으면 nil이라 서버 전체가 죽는다(#316) — 지금
	// 스트림을 고정해 넘긴다.
	stream := s.stream
	if err := runWithContext(callCtx, func() error { return stream.Send(request) }); err != nil {
		s.reset()
		return nil, s.wrapStreamError("send AI video frame", err)
	}

	var response *aiv1.ProcessedVideoChunk
	if err := runWithContext(callCtx, func() error {
		var err error
		response, err = stream.Recv()
		return err
	}); err != nil {
		s.reset()
		return nil, s.wrapStreamError("receive AI video frame", err)
	}
	return response, nil
}

// Submit은 한 프레임을 보내고 응답을 기다리지 않는다(#495). 응답은 보낸 순서대로
// 오며 돌려받은 PendingFrame의 Wait로 받는다. 앞 응답을 기다리지 않고 다음 프레임을
// 보내 AI 워커가 앞 프레임의 블러와 다음 프레임의 추론을 겹쳐 처리하게 한다. 한
// Stream에서 Process와 섞어 쓰지 않는다.
func (s *Stream) Submit(data []byte, timestamp int64, width, height uint16, pixFmt string, mosaic *aiv1.MosaicConfig) (*PendingFrame, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.ensureStream(); err != nil {
		return nil, err
	}
	stream := s.stream
	if s.receiving != stream {
		s.receiving = stream
		go s.receive(stream, s.generation)
	}
	callCtx, cancel := context.WithTimeout(s.ctx, s.timeout)
	defer cancel()
	request := s.request(data, timestamp, width, height, pixFmt, mosaic)
	if err := runWithContext(callCtx, func() error { return stream.Send(request) }); err != nil {
		err = s.wrapStreamError("send AI video frame", err)
		s.failPendingLocked(err)
		return nil, err
	}
	frame := &PendingFrame{stream: s, generation: s.generation, sentAt: time.Now(), result: make(chan pendingResult, 1)}
	s.pending = append(s.pending, frame)
	return frame, nil
}

// SubmitFrame은 Submit과 같고, 응답을 기다리는 함수를 돌려준다. media 패키지의
// AIPipeline이 이 형태로 쓴다.
func (s *Stream) SubmitFrame(data []byte, timestamp int64, width, height uint16, pixFmt string, mosaic *aiv1.MosaicConfig) (func() (*aiv1.ProcessedVideoChunk, error), error) {
	frame, err := s.Submit(data, timestamp, width, height, pixFmt, mosaic)
	if err != nil {
		return nil, err
	}
	return frame.Wait, nil
}

// Wait는 이 프레임의 응답을 기다린다. 보낸 때부터 AI_GRPC_TIMEOUT 안에 오지 않으면
// 스트림을 끊는다. 함께 보낸 나머지 요청도 같은 스트림의 응답을 받을 수 없으므로
// 모두 실패로 끝난다.
func (f *PendingFrame) Wait() (*aiv1.ProcessedVideoChunk, error) {
	s := f.stream
	timer := time.NewTimer(time.Until(f.sentAt.Add(s.timeout)))
	defer timer.Stop()
	select {
	case result := <-f.result:
		return result.response, result.err
	case <-timer.C:
		s.abandon(f.generation, s.wrapStreamError("receive AI video frame", context.DeadlineExceeded))
	case <-s.ctx.Done():
		s.abandon(f.generation, s.wrapStreamError("receive AI video frame", s.ctx.Err()))
	}
	result := <-f.result
	return result.response, result.err
}

// receive는 한 스트림의 응답을 보낸 순서대로 기다리는 요청에 넘긴다. 스트림이
// 끊기면(reset으로 generation이 바뀌면) 그 스트림의 요청은 reset한 쪽이 이미 실패로
// 끝냈으므로 조용히 멈춘다.
func (s *Stream) receive(stream aiv1.AiProcessor_ProcessVideoClient, generation uint64) {
	for {
		response, err := stream.Recv()
		s.mu.Lock()
		if s.generation != generation {
			s.mu.Unlock()
			return
		}
		if err == nil && len(s.pending) == 0 {
			err = errors.New("AI sent a response without a pending frame")
		}
		if err != nil {
			s.failPendingLocked(s.wrapStreamError("receive AI video frame", err))
			s.mu.Unlock()
			return
		}
		frame := s.pending[0]
		s.pending = s.pending[1:]
		s.mu.Unlock()
		frame.result <- pendingResult{response: response}
	}
}

// abandon은 generation 스트림이 아직 쓰이고 있으면 끊고 기다리던 요청을 모두 err로
// 끝낸다. 그 사이 응답이 와서 이미 끝난 요청에는 영향이 없다.
func (s *Stream) abandon(generation uint64, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.generation == generation {
		s.failPendingLocked(err)
	}
}

// failPendingLocked는 기다리던 요청을 모두 err로 끝내고 스트림을 끊는다. s.mu를
// 쥔 채 부른다.
func (s *Stream) failPendingLocked(err error) {
	for _, frame := range s.pending {
		frame.result <- pendingResult{err: err}
	}
	s.pending = nil
	s.reset()
}

func (s *Stream) request(data []byte, timestamp int64, width, height uint16, pixFmt string, mosaic *aiv1.MosaicConfig) *aiv1.VideoChunk {
	return &aiv1.VideoChunk{
		Data:       data,
		Timestamp:  timestamp,
		SessionId:  s.sessionID,
		OutputMode: aiv1.VideoOutputMode_VIDEO_OUTPUT_MODE_MOSAIC_JPEG,
		Width:      uint32(width),
		Height:     uint32(height),
		PixFmt:     pixFmt,
		// 합성 모드는 이후 프레임에서 생략하면 INVALID_ARGUMENT라 매 프레임 싣는다.
		AnonymizationMode: s.anonymization,
		// 블러 설정은 생략하면 그 프레임이 기본값으로 돌아가므로 매 프레임 싣는다.
		MosaicConfig: mosaic,
	}
}

func (s *Stream) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failPendingLocked(s.wrapStreamError("receive AI video frame", errStreamClosed))
}

func (s *Stream) ensureStream() error {
	if s.stream != nil {
		return nil
	}
	streamCtx, cancel := context.WithCancel(s.ctx)
	stream, err := s.client.ProcessVideo(streamCtx)
	if err != nil {
		cancel()
		return s.wrapStreamError("open AI ProcessVideo stream", err)
	}
	s.stream = stream
	s.streamCancel = cancel
	return nil
}

// wrapStreamError는 세션 ctx가 이미 취소됐으면 gRPC status 대신 ctx 오류를
// 감싼다. gRPC는 취소를 status Canceled로 돌려줘 errors.Is(context.Canceled)가
// 맞지 않고, 호출부는 서버가 스스로 끊은 요청을 AI 실패와 구분해야 한다(#297).
func (s *Stream) wrapStreamError(operation string, err error) error {
	if ctxErr := s.ctx.Err(); ctxErr != nil {
		return fmt.Errorf("%s: %w", operation, ctxErr)
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func (s *Stream) reset() {
	if s.stream != nil {
		_ = s.stream.CloseSend()
	}
	if s.streamCancel != nil {
		s.streamCancel()
	}
	s.stream = nil
	s.streamCancel = nil
	s.receiving = nil
	s.generation++
}

var errStreamClosed = errors.New("AI stream closed")

func runWithContext(ctx context.Context, operation func() error) error {
	result := make(chan error, 1)
	go func() { result <- operation() }()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
