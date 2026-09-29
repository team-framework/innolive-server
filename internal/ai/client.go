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

// maxAIRecvMsgSize satisfies the AI contract's ≥5 MiB receive floor with
// headroom for the worst-case processed frame.
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
		// Worst-case ProcessedVideoChunk (mosaic JPEG + per-face metadata) can
		// exceed gRPC's 4 MiB default receive cap; the AI contract mandates ≥5 MiB.
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

func (c *Client) NewStream(ctx context.Context, sessionID string) *Stream {
	return &Stream{ctx: ctx, client: c.client, timeout: c.timeout, sessionID: sessionID}
}

// whitelistTimeout allows far longer than the per-frame stream timeout: the
// first AddWhitelist can JIT-compile the face-recognition model (tens of
// seconds on Blackwell), and whitelist ops are rare user actions, not hot path.
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

	mu           sync.Mutex
	stream       aiv1.AiProcessor_ProcessVideoClient
	streamCancel context.CancelFunc
}

// Process는 한 프레임을 AI로 보내고 응답을 받는다. pixFmt가 비어 있지 않으면
// data는 raw 픽셀(예: "yuv420p")이며, AI가 해석할 수 있도록 width/height를 함께
// 싣는다. pixFmt가 비면 data는 자기서술적 JPEG이라 width/height는 무시된다.
func (s *Stream) Process(data []byte, timestamp int64, width, height uint16, pixFmt string) (*aiv1.ProcessedVideoChunk, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.ensureStream(); err != nil {
		return nil, err
	}

	callCtx, cancel := context.WithTimeout(s.ctx, s.timeout)
	defer cancel()
	request := &aiv1.VideoChunk{
		Data:       data,
		Timestamp:  timestamp,
		SessionId:  s.sessionID,
		OutputMode: aiv1.VideoOutputMode_VIDEO_OUTPUT_MODE_MOSAIC_JPEG,
		Width:      uint32(width),
		Height:     uint32(height),
		PixFmt:     pixFmt,
	}
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

func (s *Stream) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reset()
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
}

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
