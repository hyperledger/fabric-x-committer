/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package channel

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

type (
	// ReaderWriter helps in reading and writing to channels with context.
	// It enforces a quick release pattern so a worker will stop
	// immediately when the context is done, and won't be hanged until
	// the channel is closed (if ever).
	// By using the helper instead of using channels directly, we can make
	// sure our system never hangs on channels.
	// This is helpful for two scenarios when close(chan) can't be used:
	//  1. We want to stop the worker even if there are still items in the channel.
	//  2. There are multiple writers to a channel, so it is difficult to close the channel
	//     while avoiding possible panics.
	ReaderWriter[T any] interface {
		Reader[T]
		Writer[T]
	}
	// Reader helps in reading from channels with context.
	Reader[T any] interface {
		WithContext[T]
		Read() (T, bool)
		ReadWithTimeout(time.Duration) (T, bool)
	}
	// Writer helps in writing to channels with context.
	Writer[T any] interface {
		WithContext[T]
		Write(value T) bool
	}
	// WithContext supports fetching and updating the context.
	WithContext[T any] interface {
		Context() context.Context
		WithContext(ctx context.Context) ReaderWriter[T]
	}

	// Ready supports waiting for readiness and notifying of readiness.
	// It also supports closing to release waiters.
	// Ready uses atomic pointer indirection to allow safe Reset() while other
	// goroutines may be waiting on the channels. This prevents race conditions
	// when the internal state is replaced.
	Ready struct {
		ready atomic.Pointer[ready]
	}
	ready struct {
		ready  chan any
		closed chan any
		once   sync.Once
	}

	//nolint:containedctx // holding the context is required.
	channel[T any] struct {
		ctx    context.Context
		input  <-chan T
		output chan<- T
	}
)

// Make create a new channel with context.
func Make[T any](ctx context.Context, size int) ReaderWriter[T] {
	return NewReaderWriter[T](ctx, make(chan T, size))
}

// NewReaderWriter instantiate a ReaderWriter.
func NewReaderWriter[T any](ctx context.Context, inputOutput chan T) ReaderWriter[T] {
	return &channel[T]{
		ctx:    ctx,
		input:  inputOutput,
		output: inputOutput,
	}
}

// NewReader instantiate a Reader.
func NewReader[T any](ctx context.Context, input <-chan T) Reader[T] {
	return &channel[T]{
		ctx:   ctx,
		input: input,
	}
}

// NewWriter instantiate a Writer.
func NewWriter[T any](ctx context.Context, output chan<- T) Writer[T] {
	return &channel[T]{
		ctx:    ctx,
		output: output,
	}
}

// Context returns the internal context.
func (c *channel[T]) Context() context.Context {
	return c.ctx
}

// WithContext creates a new instance with a different context.
func (c *channel[T]) WithContext(ctx context.Context) ReaderWriter[T] {
	return &channel[T]{
		ctx:    ctx,
		input:  c.input,
		output: c.output,
	}
}

// Read one value from the channel.
// Returns false if the channel is closed or the context is done.
func (c *channel[T]) Read() (T, bool) {
	if c.input == nil || c.ctx.Err() != nil {
		return *new(T), false
	}
	select {
	case <-c.ctx.Done():
		return *new(T), false
	case val, ok := <-c.input:
		return val, ok
	}
}

// ReadWithTimeout reads one value from the channel.
// Returns false if the channel is closed, timeout occurred, or the context is done.
func (c *channel[T]) ReadWithTimeout(timeout time.Duration) (T, bool) {
	if c.input == nil || c.ctx.Err() != nil {
		return *new(T), false
	}
	select {
	case <-c.ctx.Done():
		return *new(T), false
	case <-time.After(timeout):
		return *new(T), false
	case val, ok := <-c.input:
		return val, ok
	}
}

// Write one value to the channel.
// Returns false if the context is done.
func (c *channel[T]) Write(value T) bool {
	if c.output == nil || c.ctx.Err() != nil {
		return false
	}
	select {
	case <-c.ctx.Done():
		return false
	case c.output <- value:
		return true
	}
}

// NewReady instantiate a new Ready.
func NewReady() *Ready {
	r := &Ready{}
	r.ready.Store(newReady())
	return r
}

func newReady() *ready {
	return &ready{
		ready:  make(chan any),
		closed: make(chan any),
	}
}

// Reset resets the object to be reused.
func (r *Ready) Reset() {
	rr := r.ready.Load()
	rr.once.Do(func() {
		close(rr.closed)
	})
	// We only set a new internal ready to replace the one we closed.
	// If another process already replaced it, we can continue.
	r.ready.CompareAndSwap(rr, newReady())
}

// SignalReady signals readiness.
func (r *Ready) SignalReady() {
	rr := r.ready.Load()
	rr.once.Do(func() {
		close(rr.ready)
	})
}

// Close notifies of closing.
func (r *Ready) Close() {
	rr := r.ready.Load()
	rr.once.Do(func() {
		close(rr.closed)
	})
}

// WaitForReady returns true if the object is ready,
// or false if it is closed or the context ended before that.
func (r *Ready) WaitForReady(ctx context.Context) bool {
	return WaitForAllReady(ctx, r)
}

// WaitForAllReady returns true if all objects are ready,
// or false if one of them is closed or the context ended before that.
func WaitForAllReady(ctx context.Context, ready ...*Ready) bool {
	for _, r := range ready {
		rr := r.ready.Load()
		select {
		case <-ctx.Done():
			return false
		case <-rr.closed:
			return false
		case <-rr.ready:
		}
	}
	return true
}
