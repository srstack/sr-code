package sender

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/nexustar/usher/internal/backend"
)

// loggedTurnConfig describes the small protocol-specific edges around the
// shared content-plane loop: live deltas and the terminal protocol result.
type loggedTurnConfig[R, D any] struct {
	backend   string
	idKey     string
	id        string
	cwd       string
	fresh     bool
	path      string
	offset    int64
	locate    func() string
	drainWait time.Duration
	tail      tailConfig
	done      <-chan R
	deltas    <-chan D
	delta     func(D) (kind, text string, emit bool)
	result    func(context.Context, chan<- StreamEvent, R)
	logger    *slog.Logger
}

// mergeLoggedTurn is the common Claude/Codex turn loop. Their control
// protocols start turns differently, but both merge protocol deltas and a
// terminal result with the authoritative persisted-log tail.
func mergeLoggedTurn[R, D any](ctx context.Context, cfg loggedTurnConfig[R, D]) <-chan StreamEvent {
	out := make(chan StreamEvent, 64)
	go func() {
		defer close(out)
		started, _ := json.Marshal(backend.ProcessStartedPayload{Cwd: cfg.cwd, Fresh: cfg.fresh})
		if !sendEvent(ctx, out, StreamEvent{Type: backend.EventProcessStarted, Raw: started}) {
			return
		}
		path := cfg.path
		if path == "" {
			path = cfg.locate()
		}
		if path == "" {
			emitError(ctx, out, cfg.backend+" session log did not appear after prompt")
			return
		}
		tailCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		events := tailTurn(tailCtx, path, cfg.offset, cfg.logger, cfg.tail)
		deltas := cfg.deltas
		for {
			select {
			case delta, ok := <-deltas:
				if !ok {
					deltas = nil
					continue
				}
				kind, value, emit := cfg.delta(delta)
				if emit && !emitLiveDelta(ctx, out, kind, value) {
					return
				}
			case ev, ok := <-events:
				if !ok {
					select {
					case result := <-cfg.done:
						cfg.result(ctx, out, result)
					case <-time.After(cfg.drainWait):
						cfg.logger.Warn(cfg.backend+" turn finalized from log backstop", cfg.idKey, cfg.id)
					case <-ctx.Done():
					}
					return
				}
				if !sendEvent(ctx, out, ev) {
					return
				}
			case result := <-cfg.done:
				cfg.result(ctx, out, result)
				drainTail(ctx, out, events, cancel, cfg.drainWait, cfg.logger,
					cfg.backend+" log drain timed out", cfg.idKey, cfg.id)
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}
