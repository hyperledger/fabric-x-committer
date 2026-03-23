/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package retry

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

// Sustain attempts to keep a continuous operation `op` running indefinitely.
// Unlike Execute which retries until success, Sustain is designed for long-running operations
// that should continue running until explicitly stopped or a permanent failure occurs.
//
// Operation Behavior:
//   - If op returns nil: Operation is running smoothly, Sustain resets backoff and retries immediately
//   - If op returns ErrBackOff: Transient error, Sustain applies exponential backoff before retrying
//   - If op returns ErrNonRetryable: Permanent failure, Sustain stops and returns the error
//   - If op returns any other error: Retries immediately (treated as transient)
//
// Sustain stops and returns when:
//   - op returns an error wrapping ErrNonRetryable (permanent failure),
//   - The context ctx is cancelled (returns context error),
//   - The backoff strategy times out after MaxElapsedTime of continuous ErrBackOff errors.
func Sustain(ctx context.Context, p *Profile, op func() error) error {
	b := p.NewBackoff()

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
