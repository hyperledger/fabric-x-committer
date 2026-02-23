/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package utils

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConcurrencyLimiter(t *testing.T) {
	t.Parallel()

	t.Run("LimitReached", func(t *testing.T) {
		t.Parallel()
		counter := NewConcurrencyLimiter(1)
		require.True(t, counter.TryAcquire(t.Context()))
		require.EqualValues(t, 1, counter.Load())

		require.False(t, counter.TryAcquire(t.Context()))
		require.EqualValues(t, 1, counter.Load())
	})

	t.Run("ContextCanceled", func(t *testing.T) {
		t.Parallel()
		counter := NewConcurrencyLimiter(1)
		require.True(t, counter.TryAcquire(t.Context()))

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		require.False(t, counter.TryAcquire(ctx))
		require.EqualValues(t, 1, counter.Load())
	})

	t.Run("ReleaseAndReacquire", func(t *testing.T) {
		t.Parallel()
		counter := NewConcurrencyLimiter(1)
		require.True(t, counter.TryAcquire(t.Context()))
		require.False(t, counter.TryAcquire(t.Context()))

		counter.Release()
		require.True(t, counter.TryAcquire(t.Context()))
	})

	t.Run("Unbounded", func(t *testing.T) {
		t.Parallel()
		counter := NewConcurrencyLimiter(0)
		for range 100 {
			require.True(t, counter.TryAcquire(t.Context()))
			t.Cleanup(counter.Release)
		}
	})
}
