package tgc

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tgerr"
	"go.uber.org/zap"
)

func invoker(errs ...error) (telegram.InvokeFunc, *int) {
	calls := 0
	return func(context.Context, bin.Encoder, bin.Decoder) error {
		calls++
		if calls <= len(errs) {
			return errs[calls-1]
		}
		return nil
	}, &calls
}

func TestNetRetryRecoversFromDroppedConnection(t *testing.T) {
	netRetryDelay = time.Millisecond
	internal := fmt.Errorf("rpcDoRequest: retryUntilAck: engine forcibly closed: %w", context.Canceled)
	next, calls := invoker(internal, internal)
	if err := netRetry(zap.NewNop()).Handle(next)(context.Background(), nil, nil); err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if *calls != 3 {
		t.Errorf("calls = %d, want 3", *calls)
	}
}

func TestNetRetryGivesUpWithoutLookingCancelled(t *testing.T) {
	netRetryDelay = time.Millisecond
	errs := make([]error, netRetries+5)
	for i := range errs {
		errs[i] = fmt.Errorf("invoke pool: %w", context.Canceled)
	}
	next, calls := invoker(errs...)
	err := netRetry(zap.NewNop()).Handle(next)(context.Background(), nil, nil)
	if err == nil || errors.Is(err, context.Canceled) {
		t.Fatalf("final error must not match context.Canceled: %v", err)
	}
	if *calls != netRetries {
		t.Errorf("calls = %d, want %d", *calls, netRetries)
	}
}

func TestNetRetryLeavesAPIErrorsAndRealCancelAlone(t *testing.T) {
	netRetryDelay = time.Millisecond
	apiErr := tgerr.New(400, "FILE_REFERENCE_EXPIRED")
	next, calls := invoker(apiErr)
	if err := netRetry(zap.NewNop()).Handle(next)(context.Background(), nil, nil); !errors.Is(err, apiErr) || *calls != 1 {
		t.Errorf("API error should pass through once: %v, calls=%d", err, *calls)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	next, calls = invoker(context.Canceled)
	if err := netRetry(zap.NewNop()).Handle(next)(ctx, nil, nil); !errors.Is(err, context.Canceled) || *calls != 1 {
		t.Errorf("caller cancel should stop immediately: %v, calls=%d", err, *calls)
	}
}
