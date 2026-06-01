package k8s

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"syscall"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/kubexa/kubexa-agent/internal/logger"
)

const (
	maxRetryAttempts = 3
	retryBaseDelay   = 100 * time.Millisecond
	retryMaxDelay    = 2 * time.Second
)

// isTransientError reports whether err is retryable (network timeout, 429, 503).
func isTransientError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}

	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}

	if apierrors.IsTooManyRequests(err) || apierrors.IsServiceUnavailable(err) {
		return true
	}
	if apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) {
		return true
	}

	statusErr, ok := err.(apierrors.APIStatus)
	if !ok {
		return false
	}
	switch statusErr.Status().Code {
	case 429, 503:
		return true
	default:
		return false
	}
}

// classifyErrorType returns a stable label value for Prometheus error metrics.
func classifyErrorType(err error) string {
	if err == nil {
		return "none"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if apierrors.IsTooManyRequests(err) {
		return "rate_limited"
	}
	if apierrors.IsServiceUnavailable(err) {
		return "unavailable"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	if apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) {
		return "timeout"
	}
	return "other"
}

func retryDelay(attempt int) time.Duration {
	if attempt <= 0 {
		return retryBaseDelay
	}
	delay := float64(retryBaseDelay) * math.Pow(2, float64(attempt-1))
	if delay > float64(retryMaxDelay) {
		return retryMaxDelay
	}
	return time.Duration(delay)
}

// withRetry executes fn up to maxRetryAttempts times on transient errors.
func withRetry(ctx context.Context, log *logger.Logger, operation string, fn func(context.Context) error) error {
	var lastErr error
	for attempt := 1; attempt <= maxRetryAttempts; attempt++ {
		lastErr = fn(ctx)
		if lastErr == nil || !isTransientError(lastErr) || attempt == maxRetryAttempts {
			return lastErr
		}

		if log != nil {
			log.Warn("kubernetes api call failed, retrying",
				logger.F("operation", operation),
				logger.F("attempt", attempt),
				logger.F("max_attempts", maxRetryAttempts),
				logger.F("error", lastErr.Error()),
			)
		}

		delay := retryDelay(attempt)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("kubernetes api %s: %w", operation, ctx.Err())
		case <-timer.C:
		}
	}
	return lastErr
}
