/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package retry

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestWithDefaultsMaxElapsedTime verifies the pointer semantics of MaxElapsedTime:
// nil falls back to the default, while a non-nil value (including an explicit 0
// that requests unlimited retries) is preserved. WithDefaults must be idempotent.
func TestWithDefaultsMaxElapsedTime(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		profile  *Profile
		expected time.Duration
	}{
		{name: "nil profile uses default", profile: nil, expected: defaultMaxElapsedTime},
		{name: "nil field uses default", profile: &Profile{}, expected: defaultMaxElapsedTime},
		{name: "zero is preserved (unlimited)", profile: &Profile{MaxElapsedTime: new(time.Duration(0))}, expected: 0},
		{
			name:     "positive is preserved",
			profile:  &Profile{MaxElapsedTime: new(30 * time.Second)},
			expected: 30 * time.Second,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.profile.WithDefaults()
			require.NotNil(t, got.MaxElapsedTime)
			require.Equal(t, tc.expected, *got.MaxElapsedTime)

			// WithDefaults must be idempotent: re-applying it must not change MaxElapsedTime.
			// This matters because Sustain applies WithDefaults twice along its call path.
			again := got.WithDefaults()
			require.NotNil(t, again.MaxElapsedTime)
			require.Equal(t, tc.expected, *again.MaxElapsedTime)
		})
	}
}
