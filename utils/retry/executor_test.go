/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package retry

import (
	"bytes"
	"context"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-lib-go/common/flogging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewBackoff(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name                        string
		profile                     *Profile
		expectedInitialInterval     time.Duration
		expectedRandomizationFactor float64
		expectedMultiplier          float64
		expectedMaxInterval         time.Duration
		expectedMaxElapsedTime      time.Duration
	}{
		{
			name:                        "default",
			profile:                     nil,
			expectedInitialInterval:     defaultInitialInterval,
			expectedRandomizationFactor: defaultRandomizationFactor,
			expectedMultiplier:          defaultMultiplier,
			expectedMaxInterval:         defaultMaxInterval,
			expectedMaxElapsedTime:      defaultMaxElapsedTime,
		},
		{
			name: "custom",
			profile: &Profile{
				InitialInterval:     10 * time.Millisecond,
				RandomizationFactor: 0.2,
				Multiplier:          2.0,
				MaxInterval:         50 * time.Millisecond,
				MaxElapsedTime:      100 * time.Millisecond,
			},
			expectedInitialInterval:     10 * time.Millisecond,
			expectedRandomizationFactor: 0.2,
			expectedMultiplier:          2.0,
			expectedMaxInterval:         50 * time.Millisecond,
			expectedMaxElapsedTime:      100 * time.Millisecond,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := tt.profile.WithDefaults()
			assert.InEpsilon(t, tt.expectedInitialInterval, p.InitialInterval, 0)
			assert.InEpsilon(t, tt.expectedRandomizationFactor, p.RandomizationFactor, 0)
			assert.InEpsilon(t, tt.expectedMultiplier, p.Multiplier, 0)
			assert.Equal(t, tt.expectedMaxInterval, p.MaxInterval)
			assert.Equal(t, tt.expectedMaxElapsedTime, p.MaxElapsedTime)

			b := tt.profile.NewBackoff()
			assert.InEpsilon(t, tt.expectedInitialInterval, b.InitialInterval, 0)
			assert.InEpsilon(t, tt.expectedRandomizationFactor, b.RandomizationFactor, 0)
			assert.InEpsilon(t, tt.expectedMultiplier, b.Multiplier, 0)
			assert.Equal(t, tt.expectedMaxInterval, b.MaxInterval)
		})
	}
}

func TestExecute(t *testing.T) {
	t.Parallel()
	type testCase struct {
		name                   string
		profile                *Profile
		failUntil              int // parameter for makeOp: negative means always fail
		expectedCallCount      int // expected number of calls if the op eventually succeeds;
		expectError            bool
		expectedErrorSubstring string
	}

	tests := []testCase{
		{
			name: "Success",
			profile: &Profile{
				InitialInterval: 1 * time.Millisecond,
				MaxInterval:     100 * time.Millisecond,
				MaxElapsedTime:  1 * time.Second,
			},
			failUntil:         3, // op fails until the third call, then succeeds.
			expectedCallCount: 3,
			expectError:       false,
		},
		{
			name: "Failure",
			profile: &Profile{
				InitialInterval: 10 * time.Millisecond,
				MaxInterval:     500 * time.Millisecond,
				MaxElapsedTime:  5 * time.Second,
			},
			failUntil:              -1, // op always fails.
			expectError:            true,
			expectedErrorSubstring: "error",
		},
		{
			name:              "Nil Profile",
			profile:           nil,
			failUntil:         0, // op succeeds immediately.
			expectedCallCount: 1,
			expectError:       false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			op, callCount := makeOp(tc.failUntil)
			err := Execute(t.Context(), tc.profile, op)
			if tc.expectError {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.expectedErrorSubstring)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.expectedCallCount, *callCount)
			}
		})
	}
}

// TestExecuteLogLevel is used to manually verify the log output is using the correct
// method name when logging.
//
//nolint:paralleltest // We cannot run in parallel because we modify the logger.
func TestExecuteLogLevel(t *testing.T) {
	var b bytes.Buffer
	flogging.SetWriter(&b)
	t.Cleanup(func() {
		flogging.SetWriter(os.Stderr)
	})

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	t.Cleanup(cancel)
	err := Execute(ctx, nil, func() error {
		time.Sleep(10 * time.Millisecond)
		return errors.New("Execute error")
	})
	require.Error(t, err)

	ctx, cancel = context.WithTimeout(t.Context(), time.Second)
	t.Cleanup(cancel)
	_, err = ExecuteWithResult(ctx, nil, func() (any, error) {
		time.Sleep(10 * time.Millisecond)
		return nil, errors.New("ExecuteWithResult error")
	})
	require.Error(t, err)

	ctx, cancel = context.WithTimeout(t.Context(), time.Second)
	t.Cleanup(cancel)
	res := WaitForCondition(ctx, nil, func() bool {
		time.Sleep(10 * time.Millisecond)
		return false
	})
	require.False(t, res)

	ctx, cancel = context.WithTimeout(t.Context(), time.Second)
	t.Cleanup(cancel)
	err = Sustain(ctx, nil, func() error {
		time.Sleep(10 * time.Millisecond)
		return errors.Wrap(ErrNonRetryable, "Sustain error")
	})
	require.Error(t, err)

	// Regain ownership over the buffer.
	flogging.SetWriter(os.Stderr)

	output := b.String()
	t.Log(output)

	// Remove the color codes from the log output.
	output = regexp.MustCompile(`\x1b\[[0-9;]*m`).ReplaceAllString(output, "")
	require.Contains(t, output, "[retry] TestExecuteLogLevel -> Execute error")
	require.Contains(t, output, "[retry] TestExecuteLogLevel -> ExecuteWithResult error")
	require.Contains(t, output, "[retry] TestExecuteLogLevel -> condition not satisfied")
	require.Contains(t, output, "[retry] TestExecuteLogLevel -> Sustain error")
}

// makeOp returns an operation and a pointer to a call counter.
// If failUntil is negative, the operation always fails.
// Otherwise, the op returns an error until callCount >= failUntil.
func makeOp(failUntil int) (func() error, *int) {
	callCount := 0
	op := func() error {
		callCount++
		if failUntil < 0 || callCount < failUntil {
			return errors.New("error")
		}
		return nil
	}
	return op, &callCount
}
