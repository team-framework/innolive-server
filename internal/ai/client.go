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

type Stream struct {
	ctx       context.Context
	client    aiv1.AiProcessorClient
	timeout   time.Duration
	sessionID string

	mu           sync.Mutex
	stream       aiv1.AiProcessor_ProcessVideoClient
	streamCancel context.CancelFunc
}

func (s *Stream) Process(data []byte, timestamp int64) (*aiv1.ProcessedVideoChunk, error) {
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
	}
	if err := runWithContext(callCtx, func() error { return s.stream.Send(request) }); err != nil {
		s.reset()
		return nil, fmt.Errorf("send AI video frame: %w", err)
	}

	var response *aiv1.ProcessedVideoChunk
	if err := runWithContext(callCtx, func() error {
		var err error
		response, err = s.stream.Recv()
		return err
	}); err != nil {
		s.reset()
		return nil, fmt.Errorf("receive AI video frame: %w", err)
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
		return fmt.Errorf("open AI ProcessVideo stream: %w", err)
	}
	s.stream = stream
	s.streamCancel = cancel
	return nil
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
