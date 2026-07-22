// Package retry provides a generic fixed-attempt retry loop with
// context-aware backoff, factoring out the loop mechanics shared by this
// codebase's several hand-rolled retry call sites. Retryability and backoff
// duration decisions stay with each call site's closure — only the looping,
// sleeping, and cancellation plumbing is shared here.
package retry

import (
	"context"
	"fmt"
	"time"
)

// Loop calls fn up to attempts times. fn returns whether the result was
// successful/final (retry=false) or should be retried (retry=true). Loop
// sleeps backoff(attempt) between attempts (not after the last one),
// honoring ctx cancellation during the sleep.
// If attempts <= 0, Loop returns an error without calling fn.
func Loop[T any](ctx context.Context, attempts int, backoff func(attempt int) time.Duration, fn func(attempt int) (result T, err error, retry bool)) (T, error) {
	if attempts <= 0 {
		var zero T
		return zero, fmt.Errorf("retry.Loop: attempts must be positive, got %d", attempts)
	}
	var result T
	var err error
	for attempt := 0; attempt < attempts; attempt++ {
		var retry bool
		result, err, retry = fn(attempt)
		if !retry {
			return result, err
		}
		if attempt < attempts-1 {
			timer := time.NewTimer(backoff(attempt))
			select {
			case <-ctx.Done():
				timer.Stop()
				var zero T
				return zero, ctx.Err()
			case <-timer.C:
			}
		}
	}
	return result, err
}
