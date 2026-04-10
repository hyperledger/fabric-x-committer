// Copyright IBM Corp. All Rights Reserved.
//
// SPDX-License-Identifier: Apache-2.0

package periodic

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTicker(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ch := Ticker(ctx, 50*time.Millisecond)

	tickCount := 0
	start := time.Now()
	for range ch {
		tickCount++
		if tickCount >= 3 {
			cancel() // Cancel context after 3 ticks
			break
		}
	}
	elapsed := time.Since(start)

	require.GreaterOrEqual(t, tickCount, 3, "expected at least 3 ticks")
	// Expected ~100ms for 3 ticks at 50ms intervals (t=0ms, 50ms, 100ms).
	// Use 100ms delta because goroutine scheduling can add significant
	// variance in CI/test environments.
	require.InDelta(t, float64(100*time.Millisecond),
		float64(elapsed), float64(100*time.Millisecond), "expected elapsed time ~100ms")

	// Channel should close soon after cancel
	select {
	case _, ok := <-ch:
		require.False(t, ok, "expected channel to be closed after context cancel")
	case <-time.After(100 * time.Millisecond):
	}
}
