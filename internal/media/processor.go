package media

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"inno-live-server/internal/config"
	"inno-live-server/internal/metrics"
)

type Processor struct {
	mode       config.PrivacyMode
	fixedDelay time.Duration
	metrics    *metrics.Registry
	logger     *slog.Logger
}

func NewProcessor(mode config.PrivacyMode, fixedDelay time.Duration, registry *metrics.Registry, logger *slog.Logger) (*Processor, error) {
	if logger == nil {
		logger = slog.Default()
	}
	return &Processor{
		mode:       mode,
		fixedDelay: fixedDelay,
		metrics:    registry,
		logger:     logger,
	}, nil
}

func (p *Processor) Close() {}

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
	default:
		return nil, fmt.Errorf("unsupported privacy mode %q", p.mode)
	}
}
