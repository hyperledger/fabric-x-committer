/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package connection

import (
	"fmt"
	"testing"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewBackoff(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name                        string
		profile                     *RetryProfile
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
			profile: &RetryProfile{
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
			b := tt.profile.NewBackoff()
			assert.InEpsilon(t, tt.expectedInitialInterval, b.InitialInterval, 0)
			assert.InEpsilon(t, tt.expectedRandomizationFactor, b.RandomizationFactor, 0)
			assert.InEpsilon(t, tt.expectedMultiplier, b.Multiplier, 0)
			assert.Equal(t, tt.expectedMaxInterval, b.MaxInterval)
			assert.Equal(t, tt.expectedMaxElapsedTime, b.MaxElapsedTime)
			assert.Equal(t, backoff.Stop, b.Stop)
		})
	}
}

func TestExecute(t *testing.T) {
	t.Parallel()
	type testCase struct {
		name                   string
		profile                *RetryProfile
		failUntil              int // parameter for makeOp: negative means always fail
		expectedCallCount      int // expected number of calls if the op eventually succeeds;
		expectError            bool
		expectedErrorSubstring string
	}

	tests := []testCase{
		{
			name: "Success",
			profile: &RetryProfile{
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
			profile: &RetryProfile{
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
			err := tc.profile.Execute(t.Context(), op)
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

func TestGrpcRetryJSON(t *testing.T) {
	t.Parallel()
	templateExpectedJSON := `
	{
	  "loadBalancingConfig": [{"round_robin": {}}],
	  "methodConfig": [{
		"name": [{}],
		"retryPolicy": {
		  "maxAttempts": %d,
		  "backoffMultiplier": 1.5,
		  "initialBackoff": "0.5s",
		  "maxBackoff": "10s",
		  "retryableStatusCodes": ["UNAVAILABLE", "DEADLINE_EXCEEDED", "RESOURCE_EXHAUSTED"]
		}
	  }]
	}`
	for _, tt := range []struct {
		maxElapsedTime   time.Duration
		expectedAttempts int
	}{
		{maxElapsedTime: 0, expectedAttempts: 96},
		{maxElapsedTime: 15 * time.Second, expectedAttempts: 7},
	} {
		t.Run(fmt.Sprintf("maxElapsed=%s", tt.maxElapsedTime), func(t *testing.T) {
			t.Parallel()
			profile := RetryProfile{MaxElapsedTime: tt.maxElapsedTime}
			jsonRaw := profile.MakeGrpcRetryPolicyJSON()
			require.JSONEq(t, fmt.Sprintf(templateExpectedJSON, tt.expectedAttempts), jsonRaw)
		})
	}
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

func TestCalcMaxAttempts(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		i, a, m, t float64
		n          int
	}{
		{i: 1, a: 3, m: 2, t: 3, n: 3},  // 0 + 1 + 2         = 3
		{i: 1, a: 3, m: 2, t: 6, n: 4},  // 0 + 1 + 2 + 3     = 6
		{i: 1, a: 3, m: 2, t: 9, n: 5},  // 0 + 1 + 2 + 3 + 3 = 9
		{i: 1, a: 16, m: 2, t: 7, n: 4}, // 0 + 1 + 2 + 4     = 7
	} {
		for _, e := range []float64{0, 1} {
			tc := tc
			tc.t += e
			t.Run(fmt.Sprintf("%+v", tc), func(t *testing.T) {
				t.Parallel()
				require.Equal(t, tc.n, calcMaxAttempts(tc.i, tc.a, tc.m, tc.t))
			})
		}
	}
}
