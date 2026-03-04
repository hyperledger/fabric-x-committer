/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package connection

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRateLimitConfigValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		config        *RateLimitConfig
		expectError   bool
		errorContains string
	}{
		{
			name:        "nil config is valid",
			config:      nil,
			expectError: false,
		},
		{
			name:        "disabled rate limiting is valid",
			config:      &RateLimitConfig{RequestsPerSecond: 0, Burst: 100},
			expectError: false,
		},
		{
			name:        "burst less than requests-per-second is valid",
			config:      &RateLimitConfig{RequestsPerSecond: 1000, Burst: 200},
			expectError: false,
		},
		{
			name:        "burst equal to requests-per-second is valid",
			config:      &RateLimitConfig{RequestsPerSecond: 200, Burst: 200},
			expectError: false,
		},
		{
			name:          "burst zero is invalid when rate limiting is enabled",
			config:        &RateLimitConfig{RequestsPerSecond: 100, Burst: 0},
			expectError:   true,
			errorContains: "burst must be greater than 0",
		},
		{
			name:          "burst greater than requests-per-second is invalid",
			config:        &RateLimitConfig{RequestsPerSecond: 100, Burst: 200},
			expectError:   true,
			errorContains: "burst (200) must be less than or equal to requests-per-second",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.config.Validate()
			if tt.expectError {
				require.Error(t, err)
				require.ErrorContains(t, err, tt.errorContains)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
