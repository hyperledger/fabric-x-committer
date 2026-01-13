/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package workload

import (
	"context"
	"math"
	"sync/atomic"
	"time"
)

// RateLimiter puts tokens into a bucket at a defined rate (tokens per second).
type RateLimiter struct {
	state    atomic.Pointer[limiterState]
	maxWait  time.Duration
	minBatch uint64
}

type limiterState struct {
	startTime time.Time
	rate      uint64
	taken     atomic.Int64
	maxWait   time.Duration
	minBatch  uint64
}

// NewRateLimiter controls how fast tokens can be consumed from a generator or producer while ensuring:
//   - smooth pacing,
//   - bounded waiting,
//   - and useful batch sizes.
//
// It is designed for progress-driven workloads (streaming, chunked processing, UI updates, network sends),
// where callers prefer some progress within a deadline rather than waiting indefinitely for a full request.
// The limiter is based on a token bucket with two additional guarantees:
// - a maximum wait time for partial delivery,
// - a minimum batch size for efficiency.
//
// Unlike standard rate limiters, it enforces both a latency deadline and a minimum useful batch,
// making it suitable for block- and batch-driven systems rather than per-request throttling.
func NewRateLimiter(c *LimiterConfig) *RateLimiter {
	rate := uint64(0)
	maxWait := time.Second
	minBatch := uint64(1)
	if c != nil {
		rate = c.Rate
		if c.MaxBatchWait != 0 {
			maxWait = c.MaxBatchWait
		}
		if c.MinBatchSize != 0 {
			minBatch = c.MinBatchSize
		}
	}
	logger.Debugf("Setting limit to %d requests per second.", rate)
	l := &RateLimiter{maxWait: maxWait, minBatch: minBatch}
	l.SetRate(rate)
	return l
}

// Take requests up to requested tokens from the limiter.
// The call may block for some time, and may return fewer tokens
// than requested depending on timing and configuration.
//
// The limiter chooses one of three outcomes:
//   - Full request (fast path):
//     If requested tokens can be made available within maxWait,
//     the call waits just long enough and returns exactly requested.
//   - Partial batch (deadline path):
//     If waiting maxWait yields a meaningful amount of tokens (>= minBatch),
//     the call waits maxWait and returns as many tokens as are
//     available at that time (less than requested).
//   - Minimum batch (efficiency path):
//     If even after maxWait fewer than minBatch tokens would be available,
//     the call waits longer until minBatch is ready, then returns exactly minBatch.
func (l *RateLimiter) Take(ctx context.Context, requestedTokens int64) int64 {
	return l.state.Load().take(ctx, requestedTokens)
}

// Rate returns the current rate limit in tokens per second.
func (l *RateLimiter) Rate() uint64 {
	return l.state.Load().rate
}

// SetRate updates the rate limit to the specified value in tokens per second.
// It resets the timeline, so any accumulated tokens will be erased.
func (l *RateLimiter) SetRate(rate uint64) {
	l.state.Store(&limiterState{
		startTime: time.Now(),
		rate:      rate,
		maxWait:   max(0, l.maxWait),  // ensure maxWait is non-negative.
		minBatch:  max(1, l.minBatch), // ensure minBatch is at least 1.
	})
}

// take calculates how much time should have passes for the total number of tokens to be taken.
// If not enough time has passed, it waits for the remaining time or until maxWait is reached.
// It then returns the number of tokens that can be taken within the allowed time.
func (l *limiterState) take(ctx context.Context, requestedTokens int64) int64 {
	if requestedTokens < 1 {
		return 0
	}
	if l.rate == 0 {
		return requestedTokens
	}

	// This is the linearization point which determines the place in the timeline for this request.
	// We might return some of the tokens after this point, but we do not read this value again.
	takenTokens := l.taken.Add(requestedTokens)

	// Calculate the expected duration to have taken 'takenTokens' tokens.
	expectedDuration := expectedDurationForTokens(l.rate, takenTokens)
	elapsedDuration := time.Since(l.startTime)

	// If enough time has passed, return immediately.
	if elapsedDuration > expectedDuration {
		return requestedTokens
	}

	// We want to wait only up to maxWait.
	// However, if the requestedTokens is already less or equal to minBatch, we have to wait the entire duration.
	remainingDuration := expectedDuration - elapsedDuration
	if l.maxWait > 0 && remainingDuration > l.maxWait && uint64(requestedTokens) > l.minBatch {
		// Calculate how many tokens we overtook by limiting the wait to maxWait.
		over := expectedTokensForDuration(l.rate, remainingDuration-l.maxWait)
		// We deliver the difference, or minBatch (the higher between them).
		deliveringTokens := max(requestedTokens-over, int64(l.minBatch)) //nolint:gosec // uint64 -> int64.
		if deliveringTokens < requestedTokens {
			diff := requestedTokens - deliveringTokens
			// Adjust the taken tokens to reflect the reduced tokens.
			// This ensures that future calls are not penalized.
			// We do not read takenTokens again, to avoid misplacing this request in the timeline.
			l.taken.Add(-diff)
			takenTokens -= diff
			expectedDuration = expectedDurationForTokens(l.rate, takenTokens)
			remainingDuration = expectedDuration - elapsedDuration
			requestedTokens = deliveringTokens
		}
	}
	select {
	case <-ctx.Done():
		return 0
	case <-time.After(remainingDuration):
		return requestedTokens
	}
}

// expectedDurationForTokens returns the expected duration to have the given number
// of tokens available at the specified rate.
//
// availableTokens = (duration/second) * rate
// We want requestedTokens < availableTokens
// ==> requestedTokens < (duration/second) * rate
// ==> (duration/second) > requestedTokens / rate
// ==> duration > (requestedTokens / rate) * second
// ==> duration > requestedTokens * second / rate.
func expectedDurationForTokens(rate uint64, requestedTokens int64) time.Duration {
	return time.Duration(math.Floor(float64(requestedTokens) * float64(time.Second) / float64(rate)))
}

// expectedTokensForDuration returns the expected number of tokens available
// after the given duration at the specified rate.
//
// availableTokens = (duration/second) * rate
// ==> availableTokens = duration * rate / second  .
func expectedTokensForDuration(rate uint64, duration time.Duration) int64 {
	return int64(math.Ceil(float64(duration) * float64(rate) / float64(time.Second)))
}
