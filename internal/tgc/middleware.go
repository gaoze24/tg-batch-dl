package tgc

import (
	"context"
	"fmt"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"go.uber.org/zap"
)

// Longer waits are surfaced as errors instead of silently stalling the UI.
const maxFloodWait = 10 * time.Minute

// Network retry schedule: 1s, 2s, 4s, 8s, 15s... for up to netRetries attempts (about 1.5 minutes in total).
const netRetries = 9

var netRetryDelay = time.Second // overridden in tests

// middlewares returns the chain used for the main connection and the download pool (first = outermost).
func middlewares(log *zap.Logger) []telegram.Middleware {
	return []telegram.Middleware{floodWait(log), netRetry(log)}
}

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
				if err := sleep(ctx, d+time.Second); err != nil {
					return err
				}
			}
		}
	})
}

// netRetry retries calls that failed below the Telegram API layer (dropped connection, "engine forcibly closed",
// gotd's internal "context canceled") while the caller's own context is still alive.
//
// After the last attempt the error is flattened so it no longer matches context.Canceled: tdl's downloader treats that
// as "the user cancelled" and aborts every file in the batch, not just the one that hit the broken connection.
func netRetry(log *zap.Logger) telegram.Middleware {
	return telegram.MiddlewareFunc(func(next tg.Invoker) telegram.InvokeFunc {
		return func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
			delay := netRetryDelay
			for attempt := 1; ; attempt++ {
				err := next.Invoke(ctx, input, output)
				if err == nil || ctx.Err() != nil {
					return err
				}
				if _, isAPIError := tgerr.As(err); isAPIError {
					return err // a real answer from Telegram, retrying won't change it
				}
				if attempt >= netRetries {
					return fmt.Errorf("网络连接中断（已重试 %d 次）: %s", netRetries, err.Error())
				}
				log.Info("network error, retrying", zap.Int("attempt", attempt), zap.Error(err))
				if err := sleep(ctx, delay); err != nil {
					return err
				}
				delay = min(delay*2, 15*netRetryDelay)
			}
		}
	})
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
