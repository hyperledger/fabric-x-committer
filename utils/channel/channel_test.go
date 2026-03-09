/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package channel

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type data struct {
	val int
}

func TestChannel(t *testing.T) {
	t.Parallel()

	testContext, testCancel := context.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(testCancel)

	d := &data{5}

	t.Run("reader", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(testContext)
		t.Cleanup(cancel)

		chan1 := make(chan *data, 10)
		c := NewReader(ctx, chan1)
		require.Equal(t, ctx, c.Context())

		chan1 <- d
		val, ok := c.Read()
		require.True(t, ok)
		require.Equal(t, d, val)

		wg := sync.WaitGroup{}
		wg.Go(func() {
			val, ok = c.Read()
			assert.False(t, ok)
			assert.Nil(t, val)
		})

		// Make sure we are waiting on the channel.
		time.Sleep(time.Second)
		cancel()
		wg.Wait()
	})

	t.Run("writer", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(testContext)
		t.Cleanup(cancel)

		chan1 := make(chan *data, 1)
		c := NewWriter(ctx, chan1)
		require.Equal(t, ctx, c.Context())
		require.True(t, c.Write(d))

		wg := sync.WaitGroup{}
		wg.Go(func() {
			assert.False(t, c.Write(d))
		})

		// Make sure we are waiting on the channel.
		time.Sleep(time.Second)
		cancel()
		wg.Wait()

		require.Len(t, chan1, 1)
		require.Equal(t, d, <-chan1)
	})

	t.Run("reader-writer", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(testContext)
		t.Cleanup(cancel)

		chan1 := make(chan *data)
		c := NewReaderWriter(ctx, chan1)
		require.Equal(t, ctx, c.Context())

		go func() {
			assert.True(t, c.Write(d))
		}()

		val, ok := c.Read()
		require.True(t, ok)
		require.Equal(t, d, val)

		wg := sync.WaitGroup{}
		wg.Go(func() {
			assert.False(t, c.Write(d))
		})

		// Make sure we are waiting on the channel.
		time.Sleep(time.Second)
		cancel()
		wg.Wait()

		val, ok = c.Read()
		require.False(t, ok)
		require.Nil(t, val)
	})

	t.Run("with-context", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(testContext)
		t.Cleanup(cancel)

		chan1 := make(chan *data, 10)
		c := NewReaderWriter(ctx, chan1)
		require.Equal(t, ctx, c.Context())

		require.True(t, c.Write(d))
		val, ok := c.Read()
		require.True(t, ok)
		require.Equal(t, d, val)

		ctx2, cancel2 := context.WithCancel(ctx)
		t.Cleanup(cancel2)
		c2 := c.WithContext(ctx2)
		require.Equal(t, ctx, c.Context())
		require.Equal(t, ctx2, c2.Context())

		require.True(t, c.Write(d))
		require.True(t, c2.Write(d))

		val, ok = c.Read()
		require.True(t, ok)
		require.Equal(t, d, val)

		val, ok = c2.Read()
		require.True(t, ok)
		require.Equal(t, d, val)

		cancel2()
		require.True(t, c.Write(d))
		require.False(t, c2.Write(d))

		val, ok = c.Read()
		require.True(t, ok)
		require.Equal(t, d, val)

		val, ok = c2.Read()
		require.False(t, ok)
		require.Nil(t, val)
	})
}

func TestWaitForReady(t *testing.T) {
	t.Parallel()
	testContext, testCancel := context.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(testCancel)

	t.Run("ready", func(t *testing.T) {
		t.Parallel()
		r := NewReady()
		go func() {
			assert.True(t, r.WaitForReady(testContext))
		}()
		time.Sleep(time.Second)
		r.SignalReady()
	})

	t.Run("not ready", func(t *testing.T) {
		t.Parallel()
		r := NewReady()
		timeoutCtx, timeoutCancel := context.WithTimeout(testContext, 3*time.Second)
		t.Cleanup(timeoutCancel)
		require.False(t, r.WaitForReady(timeoutCtx))
	})

	t.Run("closed", func(t *testing.T) {
		t.Parallel()
		r := NewReady()
		go func() {
			assert.False(t, r.WaitForReady(testContext))
		}()
		time.Sleep(time.Second)
		r.Close()
	})

	t.Run("reset", func(t *testing.T) {
		t.Parallel()
		r := NewReady()
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
		r := make([]*Ready, 3)
		for i := range r {
			r[i] = NewReady()
		}
		go func() {
			assert.True(t, WaitForAllReady(testContext, r...))
		}()
		time.Sleep(time.Second)
		for _, ready := range r {
			ready.SignalReady()
		}
	})

	t.Run("not all ready", func(t *testing.T) {
		t.Parallel()
		r := make([]*Ready, 3)
		for i := range r {
			r[i] = NewReady()
		}
		for _, ready := range r[1:] {
			ready.SignalReady()
		}
		timeoutCtx, timeoutCancel := context.WithTimeout(testContext, 3*time.Second)
		t.Cleanup(timeoutCancel)
		require.False(t, WaitForAllReady(timeoutCtx, r...))
	})

	t.Run("some closed", func(t *testing.T) {
		t.Parallel()
		r := make([]*Ready, 3)
		for i := range r {
			r[i] = NewReady()
		}
		for _, ready := range r[2:] {
			ready.SignalReady()
		}
		go func() {
			assert.False(t, WaitForAllReady(testContext, r...))
		}()
		time.Sleep(time.Second)
		r[0].Close()
	})
}
