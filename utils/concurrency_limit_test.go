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
		require.NoError(t, counter.TryAcquire(t.Context()))
		require.Equal(t, int64(1), counter.Load())

		err := counter.TryAcquire(t.Context())
		require.ErrorIs(t, err, ErrConcurrencyLimitReached)
		require.Equal(t, int64(1), counter.Load())
	})

	t.Run("ContextCanceled", func(t *testing.T) {
		t.Parallel()
		counter := NewConcurrencyLimiter(1)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		err := counter.TryAcquire(ctx)
		require.ErrorIs(t, err, context.Canceled)
		require.Equal(t, int64(0), counter.Load())
	})

	t.Run("Unbounded", func(t *testing.T) {
		t.Parallel()
		counter := NewConcurrencyLimiter(0)
		require.NoError(t, counter.TryAcquire(t.Context()))
		require.Equal(t, int64(1), counter.Load())
	})

	t.Run("SetLimit", func(t *testing.T) {
		t.Parallel()
		counter := NewConcurrencyLimiter(1)
		require.NoError(t, counter.TryAcquire(t.Context()))
		require.ErrorIs(t, counter.TryAcquire(t.Context()), ErrConcurrencyLimitReached)

		counter.SetLimit(0)
		require.NoError(t, counter.TryAcquire(t.Context()))
		require.Equal(t, int64(2), counter.Load())
	})
}
