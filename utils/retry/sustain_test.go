/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package retry

import (
	"context"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/require"
)

func TestSustain(t *testing.T) {
	t.Parallel()

	// Cases where operation runs continuously until context cancellation
	for _, tc := range []struct {
		name          string
		operation     func(callCount uint64) error
		profile       *Profile
		cancelAfter   time.Duration
		minCallCount  uint64
		errorContains string
	}{
		{
			name:      "operation runs continuously returning nil until context cancelled",
			operation: func(uint64) error { return nil },
			profile: &Profile{
				InitialInterval: 10 * time.Millisecond,
				MaxElapsedTime:  100 * time.Millisecond,
			},
			cancelAfter:   50 * time.Millisecond,
			minCallCount:  2,
			errorContains: "context has been cancelled",
		},
		{
			name: "operation recovers from backoff errors and continues running",
			operation: func(callCount uint64) error {
				if callCount <= 3 {
					return errors.Wrap(ErrBackOff, "transient error")
				}
				return nil
			},
			profile: &Profile{
				InitialInterval: 10 * time.Millisecond,
				MaxElapsedTime:  500 * time.Millisecond,
			},
			cancelAfter:   200 * time.Millisecond,
			minCallCount:  4,
			errorContains: "context has been cancelled",
		},
		{
			name: "operation recovers from backoff errors and continues running and than back offs again",
			operation: func(callCount uint64) error {
				if callCount <= 3 || callCount >= 7 {
					return errors.Wrap(ErrBackOff, "transient error")
				}
				return nil
			},
			profile: &Profile{
				InitialInterval: 10 * time.Millisecond,
				MaxElapsedTime:  500 * time.Millisecond,
			},
			cancelAfter:   200 * time.Millisecond,
			minCallCount:  4,
			errorContains: "context has been cancelled",
		},
		{
			name: "other errors retry immediately without backoff",
			operation: func(callCount uint64) error {
				if callCount <= 3 {
					return errors.New("some transient error")
				}
				return nil
			},
			profile: &Profile{
				InitialInterval: 10 * time.Millisecond,
				MaxElapsedTime:  500 * time.Millisecond,
			},
			cancelAfter:   400 * time.Millisecond,
			minCallCount:  4,
			errorContains: "context has been cancelled",
		},
		{
			name: "max time elapsed before context cancelled",
			operation: func(uint64) error {
				return errors.Wrap(ErrBackOff, "transient error")
			},
			profile: &Profile{
				InitialInterval: 10 * time.Millisecond,
				MaxElapsedTime:  100 * time.Millisecond,
			},
			cancelAfter:   time.Second,
			minCallCount:  2,
			errorContains: "transient error",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), tc.cancelAfter)
			t.Cleanup(cancel)

			var callCount uint64
			err := Sustain(ctx, tc.profile, func() error {
				callCount++
				time.Sleep(10 * time.Millisecond)
				return tc.operation(callCount)
			})

			// Should run continuously until cancelled by context.
			require.ErrorContains(t, err, tc.errorContains)
			require.GreaterOrEqual(t, callCount, tc.minCallCount)
		})
	}

	// Failure cases
	for _, tc := range []struct {
		name          string
		operation     func() error
		profile       *Profile
		timeout       time.Duration
		expectedError string
	}{
		{
			name: "non-retryable error stops immediately",
			operation: func() error {
				return errors.Wrap(ErrNonRetryable, "permanent failure")
			},
			profile: &Profile{
				InitialInterval: 10 * time.Millisecond,
				MaxElapsedTime:  100 * time.Millisecond,
			},
			timeout:       200 * time.Millisecond,
			expectedError: "cannot recover from error",
		},
		{
			name: "context cancellation stops retry",
			operation: func() error {
				return errors.Wrap(ErrBackOff, "transient error")
			},
			profile: &Profile{
				InitialInterval: 10 * time.Millisecond,
				MaxElapsedTime:  5 * time.Second,
			},
			timeout:       50 * time.Millisecond,
			expectedError: "context has been cancelled",
		},
		{
			name: "nil profile uses defaults",
			operation: func() error {
				return errors.Wrap(ErrNonRetryable, "permanent failure")
			},
			profile:       nil,
			timeout:       200 * time.Millisecond,
			expectedError: "cannot recover from error",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), tc.timeout)
			t.Cleanup(cancel)

			err := Sustain(ctx, tc.profile, tc.operation)
			require.Error(t, err)
			require.ErrorContains(t, err, tc.expectedError)
		})
	}
}
