package app

import (
	"context"
	"log/slog"
	"time"

	"go.uber.org/fx"

	"github.com/ruinedprince/backend-challenge-go/internal/core"
)

// ReferenceResolver resumes reversals whose original operation had not yet
// arrived. Its state is in PostgreSQL, so restarting this worker loses no work.
type ReferenceResolver struct {
	processor *core.Processor
	logger    *slog.Logger
	cancel    context.CancelFunc
	done      chan struct{}
}

func NewReferenceResolver(processor *core.Processor, logger *slog.Logger) *ReferenceResolver {
	return &ReferenceResolver{processor: processor, logger: logger, done: make(chan struct{})}
}

func RegisterReferenceResolver(lifecycle fx.Lifecycle, resolver *ReferenceResolver) {
	lifecycle.Append(fx.Hook{OnStart: resolver.Start, OnStop: resolver.Stop})
}

func (r *ReferenceResolver) Start(context.Context) error {
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	go r.loop(ctx)
	return nil
}

func (r *ReferenceResolver) Stop(ctx context.Context) error {
	if r.cancel != nil {
		r.cancel()
	}
	select {
	case <-r.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *ReferenceResolver) loop(ctx context.Context) {
	defer close(r.done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if err := r.processor.ResolvePendingReferences(ctx, 10); err != nil && ctx.Err() == nil {
			r.logger.Error("resolve pending references", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
