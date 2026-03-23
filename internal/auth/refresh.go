package auth

import (
	"context"
	"fmt"
	"time"

	log "github.com/sirupsen/logrus"
)

// RefreshWithRetry retries the given refresh function up to maxRetries times with linear backoff.
// If isNonRetryable is non-nil, it is called on each error to decide whether to stop early.
func RefreshWithRetry[T any](
	ctx context.Context,
	refreshToken string,
	maxRetries int,
	refreshFn func(ctx context.Context, refreshToken string) (T, error),
	isNonRetryable func(error) bool,
) (T, error) {
	var lastErr error
	var zero T
	for attempt := range maxRetries {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return zero, ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
		result, err := refreshFn(ctx, refreshToken)
		if err == nil {
			return result, nil
		}
		if isNonRetryable != nil && isNonRetryable(err) {
			log.Warnf("Token refresh attempt %d failed with non-retryable error: %v", attempt+1, err)
			return zero, err
		}
		lastErr = err
		log.Warnf("Token refresh attempt %d failed: %v", attempt+1, err)
	}
	return zero, fmt.Errorf("token refresh failed after %d attempts: %w", maxRetries, lastErr)
}
