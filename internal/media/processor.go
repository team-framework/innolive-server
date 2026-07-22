package media

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	aiv1 "inno-live-server/api/gen/aiv1"
	"inno-live-server/internal/config"
	"inno-live-server/internal/metrics"
)

type AIStream interface {
	Process(data []byte, timestamp int64, width, height uint16, pixFmt string) (*aiv1.ProcessedVideoChunk, error)
	Close()
}

type Processor struct {
	mode       config.PrivacyMode
	fixedDelay time.Duration
	ai         AIStream
	metrics    *metrics.Registry
	logger     *slog.Logger
	wireFormat config.WireFormat
}

func NewProcessor(mode config.PrivacyMode, fixedDelay time.Duration, ai AIStream, registry *metrics.Registry, logger *slog.Logger, wireFormat config.WireFormat) (*Processor, error) {
	if mode == config.PrivacyModeReal && ai == nil {
		return nil, errors.New("real privacy mode requires an AI stream")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if wireFormat == "" {
		wireFormat = config.WireFormatJPEG
	}
	return &Processor{
		mode:       mode,
		fixedDelay: fixedDelay,
		ai:         ai,
		metrics:    registry,
		logger:     logger,
		wireFormat: wireFormat,
	}, nil
}

func (p *Processor) Close() {
	if p.ai != nil {
		p.ai.Close()
	}
}

func (p *Processor) Process(ctx context.Context, frame []byte, timestamp int64, width, height uint16) ([]byte, error) {
	startedAt := time.Now()

	switch p.mode {
	case config.PrivacyModeBypass:
		defer func() { p.metrics.ObserveProcessing(string(p.mode), time.Since(startedAt)) }()
		return frame, nil
	case config.PrivacyModeFixedDelay:
		defer func() { p.metrics.ObserveProcessing(string(p.mode), time.Since(startedAt)) }()
		timer := time.NewTimer(p.fixedDelay)
		defer timer.Stop()
		select {
		case <-timer.C:
			return frame, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	case config.PrivacyModeReal:
		return p.ProcessImage(frame, timestamp, width, height)
	default:
		return nil, fmt.Errorf("unsupported privacy mode %q", p.mode)
	}
}

func (p *Processor) ProcessImage(frame []byte, timestamp int64, width, height uint16) ([]byte, error) {
	startedAt := time.Now()
	defer func() { p.metrics.ObserveProcessing(string(p.mode), time.Since(startedAt)) }()
	if p.mode != config.PrivacyModeReal {
		return nil, fmt.Errorf("ProcessDecoded is only valid in real mode")
	}

	pixFmt := ""
	if p.wireFormat == config.WireFormatRaw {
		pixFmt = "yuv420p"
	}
	aiStartedAt := time.Now()
	response, err := p.ai.Process(frame, timestamp, width, height, pixFmt)
	p.metrics.ObserveAI(string(p.mode), time.Since(aiStartedAt))
	p.metrics.ObserveStage("grpc", time.Since(aiStartedAt))
	if err != nil {
		return nil, err
	}
	if response.GetTimestamp() != timestamp {
		return nil, fmt.Errorf("AI response timestamp mismatch: sent=%d received=%d", timestamp, response.GetTimestamp())
	}
	if !strings.EqualFold(response.GetStatusMessage(), "success") {
		return nil, fmt.Errorf("AI processing failed: status=%q", response.GetStatusMessage())
	}
	if len(response.GetData()) == 0 {
		return nil, errors.New("AI processing returned an empty frame")
	}
	return response.GetData(), nil
}
