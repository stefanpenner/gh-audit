package github

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"time"
)

// retryWithBackoff retries fn with exponential backoff and jitter.
// Backoff starts at 1s and doubles each attempt, capped at 30s.
// Jitter of +/-25% is applied to each delay.
func retryWithBackoff(ctx context.Context, logger *slog.Logger, maxRetries int, fn func() error) error {
	var lastErr error
	for attempt := range maxRetries + 1 {
		lastErr = fn()
		if lastErr == nil {
			return nil
		}

		if attempt == maxRetries {
			break
		}

		if ctx.Err() != nil {
			return fmt.Errorf("context cancelled during retry: %w", ctx.Err())
		}

		baseDelay := math.Min(float64(time.Second)*math.Pow(2, float64(attempt)), float64(30*time.Second))
		jitter := baseDelay * 0.25 * (2*rand.Float64() - 1) // +/-25%
		delay := time.Duration(baseDelay + jitter)

		logger.Warn("retrying after error",
			"attempt", attempt+1,
			"max_retries", maxRetries,
			"delay", delay,
			"error", lastErr,
		)

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("context cancelled during retry backoff: %w", ctx.Err())
		case <-timer.C:
		}
	}
	return fmt.Errorf("all %d retries exhausted: %w", maxRetries, lastErr)
}
