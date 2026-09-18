package tgc

import (
	"context"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"go.uber.org/zap"
)

// Longer waits are surfaced as errors instead of silently stalling the UI.
const maxFloodWait = 10 * time.Minute

// floodWait sleeps through FLOOD_WAIT_X and retries, so bursts (thumbnails, long listings) slow down instead of failing.
func floodWait(log *zap.Logger) telegram.Middleware {
	return telegram.MiddlewareFunc(func(next tg.Invoker) telegram.InvokeFunc {
		return func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
			for {
				err := next.Invoke(ctx, input, output)
				d, ok := tgerr.AsFloodWait(err)
				if !ok || d > maxFloodWait {
					return err
				}
				log.Info("flood wait", zap.Duration("wait", d))
				t := time.NewTimer(d + time.Second)
				select {
				case <-ctx.Done():
					t.Stop()
					return ctx.Err()
				case <-t.C:
				}
			}
		}
	})
}
