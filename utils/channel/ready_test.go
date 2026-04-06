/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package channel_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/utils/channel"
)

func TestWaitForReady(t *testing.T) {
	t.Parallel()
	testContext, testCancel := context.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(testCancel)

	t.Run("ready", func(t *testing.T) {
		t.Parallel()
		r := channel.NewReady()
		go func() {
			assert.True(t, r.WaitForReady(testContext))
		}()
		time.Sleep(time.Second)
		r.SignalReady()
	})

	t.Run("not ready", func(t *testing.T) {
		t.Parallel()
		r := channel.NewReady()
		timeoutCtx, timeoutCancel := context.WithTimeout(testContext, 3*time.Second)
		t.Cleanup(timeoutCancel)
		require.False(t, r.WaitForReady(timeoutCtx))
	})

	t.Run("closed", func(t *testing.T) {
		t.Parallel()
		r := channel.NewReady()
		go func() {
			assert.False(t, r.WaitForReady(testContext))
		}()
		time.Sleep(time.Second)
		r.Close()
	})

	t.Run("reset", func(t *testing.T) {
		t.Parallel()
		r := channel.NewReady()
		go func() {
			assert.False(t, r.WaitForReady(testContext))
		}()
		time.Sleep(time.Second)
		r.Reset()
		go func() {
			assert.True(t, r.WaitForReady(testContext))
		}()
		time.Sleep(time.Second)
		r.SignalReady()
	})

	t.Run("all ready", func(t *testing.T) {
		t.Parallel()
		r := make([]*channel.Ready, 3)
		for i := range r {
			r[i] = channel.NewReady()
		}
		go func() {
			assert.True(t, channel.WaitForAllReady(testContext, r...))
		}()
		time.Sleep(time.Second)
		for _, ready := range r {
			ready.SignalReady()
		}
	})

	t.Run("not all ready", func(t *testing.T) {
		t.Parallel()
		r := make([]*channel.Ready, 3)
		for i := range r {
			r[i] = channel.NewReady()
		}
		for _, ready := range r[1:] {
			ready.SignalReady()
		}
		timeoutCtx, timeoutCancel := context.WithTimeout(testContext, 3*time.Second)
		t.Cleanup(timeoutCancel)
		require.False(t, channel.WaitForAllReady(timeoutCtx, r...))
	})

	t.Run("some closed", func(t *testing.T) {
		t.Parallel()
		r := make([]*channel.Ready, 3)
		for i := range r {
			r[i] = channel.NewReady()
		}
		for _, ready := range r[2:] {
			ready.SignalReady()
		}
		go func() {
			assert.False(t, channel.WaitForAllReady(testContext, r...))
		}()
		time.Sleep(time.Second)
		r[0].Close()
	})
}

// TestReadyConcurrentResetAndSignal should be run with the race detector enabled: go test -race.
func TestReadyConcurrentResetAndSignal(t *testing.T) {
	t.Parallel()
	testContext, testCancel := context.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(testCancel)

	r := channel.NewReady()
	var wg sync.WaitGroup
	var successCount atomic.Uint64

	// Launch multiple goroutines that reset, signal, and wait concurrently.
	for range 1_000 {
		wg.Go(func() {
			r.Reset()
		})
		wg.Go(func() {
			r.SignalReady()
		})
		wg.Go(func() {
			if r.WaitForReady(testContext) {
				successCount.Add(1)
			}
		})
	}
	wg.Wait()

	// At least one waiter should have succeeded since we're signaling ready multiple times.
	require.Positive(t, successCount.Load(), "expected at least one successful wait")
}
