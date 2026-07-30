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

// preflightDim is the side length of the tiny synthetic probe frame. Content is
// irrelevant; only that the AI server can decode the wire format and complete a
// real round trip.
const preflightDim = 16

// Preflight verifies the full ProcessVideo round trip against every AI worker
// before any user session runs. For each target it sends one synthetic frame
// matching wireFormat and requires the worker to echo the timestamp, return
// status_message=="success" (case-insensitive), and send non-empty data — the
// same contract Processor.ProcessImage enforces per frame. This turns a silent,
// permanent, per-session blackout (e.g. a raw default paired with a JPEG-only
// PIL/OpenCV AI server) into a loud boot-time failure, and doubles as a warmup
// call that pre-pays a real model's first-inference JIT cost. The per-target
// deadline is taken from timeout (intentionally separate from the steady-state
// AI_GRPC_TIMEOUT so it can absorb first-inference warmup).
func (p *Pool) Preflight(ctx context.Context, wireFormat string, timeout time.Duration) error {
	var errs []error
	for _, client := range p.clients {
		callCtx, cancel := context.WithTimeout(ctx, timeout)
		err := client.Preflight(callCtx, wireFormat)
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

// Preflight runs one synthetic ProcessVideo round trip against this worker. The
// caller controls the deadline via ctx.
func (c *Client) Preflight(ctx context.Context, wireFormat string) error {
	if wireFormat == "raw" {
		return errors.New("AI_FRAME_WIRE_FORMAT=raw is not supported by this AI server's proto (no width/height/pix_fmt fields) — use jpeg")
	}
	data, err := syntheticFrame()
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

// syntheticFrame builds a minimal grayscale JPEG probe frame. Only jpeg is
// supported: this AI server's proto has no width/height/pix_fmt fields, so a
// raw yuv420p wire format cannot be conveyed at all (Client.Preflight rejects
// wireFormat=="raw" before calling this).
func syntheticFrame() ([]byte, error) {
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, image.NewGray(image.Rect(0, 0, preflightDim, preflightDim)), &jpeg.Options{Quality: 75}); err != nil {
		return nil, fmt.Errorf("encode preflight jpeg: %w", err)
	}
	return encoded.Bytes(), nil
}
