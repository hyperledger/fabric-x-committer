/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package connection

import (
	"context"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/cockroachdb/errors"
)

var (
	// ErrRetryTimeout is returned if the retry attempts were exhausted due to timeout.
	ErrRetryTimeout = errors.New("retry timed out")
	// ErrNonRetryable represents an error that should not trigger a retry.
	// It's used to wrap an underlying error condition when an operation
	// fails and retrying would be useless.
	// The code performing retries will check for this error type
	// (e.g., using errors.Is) and stop the retry loop if found.
	ErrNonRetryable = errors.New("cannot recover from error")
	// ErrBackOff is returned for transient errors specifically on an existing connection,
	// implying the initial connection was successful. Retry the operation after a backoff delay.
	ErrBackOff = errors.New("backoff required before retrying")
)

// Sustain attempts to keep an operation `op` running successfully against the connection,
// handling transient issues (like ErrBackOff) via retry with backoff.
//
// It stops retrying if:
//   - op returns an error wrapping ErrNonRetryable (permanent failure).
//   - The context ctx is cancelled.
//   - The internal backoff strategy times out (via ErrRetryTimeout).
func Sustain(ctx context.Context, r *RetryProfile, op func() error) error {
	if r == nil {
		r = &RetryProfile{}
	}
	b := r.NewBackoff()

	for ctx.Err() == nil {
		opErr := op()
		logger.Warnf("Sustained operation error: %s", opErr)
		if errors.Is(opErr, ErrNonRetryable) {
			return opErr
		}
		if !errors.Is(opErr, ErrBackOff) {
			b.Reset()
		}
		if err := WaitForNextBackOffDuration(ctx, b); err != nil {
			return err
		}
	}

	return errors.Wrap(ctx.Err(), "context has been cancelled")
}

// WaitForNextBackOffDuration waits for the next backoff duration.
// It stops if the context ends.
// If the backoff should stop, it returns ErrRetryTimeout.
func WaitForNextBackOffDuration(ctx context.Context, b *backoff.ExponentialBackOff) error {
	waitTime := b.NextBackOff()
	if waitTime == backoff.Stop {
		return ErrRetryTimeout
	}

	logger.Infof("Waiting [%v] before retrying", waitTime)
	select {
	case <-ctx.Done():
	case <-time.After(waitTime):
	}

	return nil
}
