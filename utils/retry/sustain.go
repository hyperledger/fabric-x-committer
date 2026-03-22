/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package retry

import (
	"context"

	"github.com/cenkalti/backoff/v5"
	"github.com/cockroachdb/errors"
)

var (
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
	p = p.WithDefaults()

	for ctx.Err() == nil {
		_, err := executeWithResult(ctx, p, func() (any, error) {
			err := op()
			if err == nil {
				err = errors.New("sustained operation ended unexpectedly")
			}
			if !errors.Is(err, ErrBackOff) {
				return nil, backoff.Permanent(err)
			}
			return nil, errors.WithMessage(err, "sustained operation error")
		})

		if errors.Is(err, ErrNonRetryable) {
			return err
		}
	}

	return errors.Wrap(ctx.Err(), "context has been cancelled")
}
