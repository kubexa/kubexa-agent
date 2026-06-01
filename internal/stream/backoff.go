package stream

import (
	"context"
	"math/rand"
	"time"
)

const backoffJitterFraction = 0.2

// backoff computes reconnect delay for attempt (0-based) with exponential growth and ±20% jitter.
func backoff(attempt int, initial, max time.Duration, rng *rand.Rand) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	delay := initial
	for i := 0; i < attempt; i++ {
		delay *= 2
		if delay >= max {
			delay = max
			break
		}
	}
	if delay > max {
		delay = max
	}
	if rng == nil {
		rng = rand.New(rand.NewSource(time.Now().UnixNano())) //nolint:gosec
	}
	// jitter in [1-0.2, 1+0.2]
	jitter := 1 - backoffJitterFraction + rng.Float64()*(2*backoffJitterFraction)
	return time.Duration(float64(delay) * jitter)
}

// sleeper waits for d or until ctx is cancelled.
type sleeper func(ctx context.Context, d time.Duration) error

func defaultSleeper(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
