package service

import (
	"context"
	"errors"
	"time"

	"github.com/gbschedule/gbschedule/internal/repository"
	"gorm.io/gorm"
)

// maxVersionTxRetries bounds retries of version-writing transactions. The
// version-number assignment is "count + 1" protected by a unique index, so two
// racing transactions can pick the same number: the loser re-counts inside a
// fresh transaction instead of surfacing a spurious 409.
const maxVersionTxRetries = 10

// retryOnTxContention reruns a write transaction while it fails on a version
// number collision (unique index on plan_id+version_no) or a transient
// database lock. Other errors abort immediately.
func (s *versionService) retryOnTxContention(ctx context.Context, fn func(tx *gorm.DB) error) error {
	var lastErr error
	for attempt := 0; attempt < maxVersionTxRetries; attempt++ {
		err := s.plans.Transaction(ctx, fn)
		if err == nil {
			return nil
		}
		lastErr = err
		if !errors.Is(err, repository.ErrConstraint) && !repository.IsLockBusyError(err) {
			return err
		}
		// Exponential backoff with a small bounded jitter from the attempt
		// number so competing goroutines do not retry in lockstep.
		wait := time.Duration(2<<uint(attempt))*time.Millisecond + time.Duration(attempt%3)*time.Millisecond
		if wait > 80*time.Millisecond {
			wait = 80 * time.Millisecond
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
	return lastErr
}
