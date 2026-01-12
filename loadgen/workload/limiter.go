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

type (
	// ConsumerRateController controls the consumption rate from a inputQueue while ensuring:
	//   - smooth pacing,
	//   - bounded waiting,
	//   - and useful batch sizes.
	//
	// It is designed for progress-driven workloads (streaming, chunked processing, UI updates, network sends),
	// where callers prefer some progress within a deadline rather than waiting indefinitely for a full request.
	// The rate controller is based on a token bucket with two additional guarantees:
	// - a maximum wait time for partial delivery,
	// - a minimum batch size for efficiency.
	//
	// Unlike standard rate limiters, it enforces both a latency deadline and a minimum useful batch,
	// making it suitable for block- and batch-driven systems rather than per-request throttling.
	ConsumerRateController[T any] struct {
		state        *atomic.Pointer[rateControllerState[T]]
		inputQueue   <-chan []T
		outputBuffer []T
	}

	// ConsumeParameters describes the consume request parameters.
	ConsumeParameters struct {
		RequestedItems uint64
		MinItems       uint64
		SoftTimeout    time.Duration
	}

	rateControllerState[T any] struct {
		startTime time.Time
		rate      uint64
		taken     atomic.Int64
	}
)

// NewConsumerRateController create a new rate controller.
func NewConsumerRateController[T any](rate uint64, inputQueue <-chan []T) *ConsumerRateController[T] {
	l := &ConsumerRateController[T]{
		state:      new(atomic.Pointer[rateControllerState[T]]),
		inputQueue: inputQueue,
	}
	l.SetRate(rate)
	return l
}

// Rate returns the current rate limit in tokens per second.
func (l *ConsumerRateController[T]) Rate() uint64 {
	return l.state.Load().rate
}

// SetRate updates the rate limit to the specified value in tokens per second.
// It resets the timeline, so any accumulated tokens will be erased.
func (l *ConsumerRateController[T]) SetRate(rate uint64) {
	logger.Debugf("Setting limit to %d requests per second.", rate)
	l.state.Store(&rateControllerState[T]{
		startTime: time.Now(),
		rate:      rate,
	})
}

// InstantiateWorker creates a new rate controller instance that shares the same state and inputQueue.
// The worker instance maintains its own outputBuffer.
func (l *ConsumerRateController[T]) InstantiateWorker() *ConsumerRateController[T] {
	return &ConsumerRateController[T]{
		state:      l.state,
		inputQueue: l.inputQueue,
	}
}

// Next consumes a single item from the inputQueue.
func (l *ConsumerRateController[T]) Next(ctx context.Context) T {
	ret := l.Consume(ctx, ConsumeParameters{RequestedItems: 1})
	if len(ret) == 0 {
		return *new(T)
	}
	return ret[0]
}

// Consume requests up to RequestedItems from the producer.
// The call may block for some time, and may return fewer items
// than requested depending on MinItems and softTimeout.
//
// The rate controller chooses one of three outcomes:
//   - Full request (fast path):
//     If RequestedItems can be made available within SoftTimeout,
//     the call waits just long enough and returns exactly the RequestedItems.
//   - Partial batch (deadline path):
//     If waiting SoftTimeout yields a meaningful amount of items (>= MinItems),
//     the call waits SoftTimeout and returns as many items as are
//     available at that time (less than requested).
//   - Minimum batch (efficiency path):
//     If even after SoftTimeout fewer than MinItems would be available,
//     the call waits longer until MinItems is ready, then returns exactly MinItems.
//
// Notes:
//   - If SoftTimeout is not positive, or MinItems is not less than RequestedItems,
//     the call waits indefinitely for RequestedItems.
//   - If the context is canceled before any items are available, the call returns nil.
//   - If RequestedItems is less than 1, the call returns nil immediately.
func (l *ConsumerRateController[T]) Consume(ctx context.Context, p ConsumeParameters) []T {
	if p.RequestedItems < 1 || l.inputQueue == nil {
		return nil
	}
	if p.SoftTimeout <= 0 {
		p.SoftTimeout = time.Duration(math.MaxInt64)
	}

	// We load the state to ensure consistent view during this call.
	s := l.state.Load()

	// This is the linearization point which determines the place in the timeline for this request.
	// We might refund some of the tokens after this point, but we do not read this value again.
	acquiredTokens := p.RequestedItems
	totalAcquiredTokens := uint64(s.taken.Add(int64(acquiredTokens))) //nolint:gosec // uint64 -> int64.
	callStart := time.Now()

	// We have to wait for the items to be available regardless of the defined rate limit.
	l.consumeToCache(ctx, acquiredTokens, p.MinItems, p.SoftTimeout)

	// At this point, either:
	// - the outputBuffer contains the at least requested number of items,
	// - the soft timeout was passed and outputBuffer contains at least the minimal number of items,
	// - the context was canceled,
	// - or the inputQueue was closed.
	if s.rate == 0 {
		// Even if the rate is unlimited, we still might be limited by the producer.
		// So we consume whatever is available in the outputBuffer at this point.
		return l.consumeFromCache(acquiredTokens, s)
	}

	// Now that we have enough items in the outputBuffer, we calculate how long we need to wait
	// to respect the rate limit.
	// We calculate the required age to have acquired 'totalAcquiredTokens' tokens.
	requiredAge := requiredDurationToProduceTokens(s.rate, totalAcquiredTokens)
	requiredWait := s.ageGap(requiredAge, callStart)
	timeSinceCallStart := time.Since(callStart)
	if requiredWait < timeSinceCallStart {
		// If there is no need to wait, we can consume whatever is available in the outputBuffer at this point.
		return l.consumeFromCache(acquiredTokens, s)
	}

	// Adjust the acquired tokens according to the current rate limit.
	// In case we overshot the soft timeout due to a slow processing, we take timeSinceCallStart as the soft timeout.
	adjustedAcquiredTokens := s.adjustAcquiredTokens(
		acquiredTokens, p.MinItems, requiredWait, max(p.SoftTimeout, timeSinceCallStart),
	)
	if adjustedAcquiredTokens < acquiredTokens {
		refund := acquiredTokens - adjustedAcquiredTokens
		// Refund the acquired tokens to reflect the reduced items.
		// This ensures that future calls are not penalized.
		s.refund(refund)
		// We do not read totalAcquiredTokens again, to avoid misplacing this request in the timeline.
		totalAcquiredTokens -= refund
		requiredAge = requiredDurationToProduceTokens(s.rate, totalAcquiredTokens)
		acquiredTokens = adjustedAcquiredTokens
	}

	// Try to fill the outputBuffer while waiting.
	// If the soft timeout was already reached, the ageGapNow will be negative.
	l.consumeToCache(ctx, acquiredTokens, p.MinItems, s.ageGapNow(requiredAge))

	// Wait the remaining time.
	select {
	case <-time.After(s.ageGapNow(requiredAge)):
		return l.consumeFromCache(acquiredTokens, s)
	case <-ctx.Done():
		s.refund(acquiredTokens)
		return nil
	}
}

// consumeToCache tries to fill the outputBuffer up to requestedItems.
// If softTimeout is reached, it tries to get at least minItems, waiting indefinitely if required.
// If softTimeout is zero or negative, it waits for minItems.
// The function returns when the outputBuffer contains enough items, or the inputQueue/context is closed.
func (l *ConsumerRateController[T]) consumeToCache(
	ctx context.Context, requestedItems, minItems uint64, softTimeout time.Duration,
) {
	softDeadlineTimer := time.NewTimer(softTimeout)
	defer softDeadlineTimer.Stop()
	softDeadline := softDeadlineTimer.C
	waitForItems := requestedItems
	for ctx.Err() == nil && uint64(len(l.outputBuffer)) < waitForItems {
		select {
		case <-ctx.Done():
			return
		case <-softDeadline:
			// We adjust the number if required items and disable the soft deadline for the following iterations.
			waitForItems = max(1, min(minItems, requestedItems))
			softDeadline = nil
		case newBatch, ok := <-l.inputQueue:
			if !ok || len(newBatch) == 0 {
				// Queue closed or producer is done.
				l.inputQueue = nil
				return
			}
			l.outputBuffer = append(l.outputBuffer, newBatch...)
		}
	}
}

func (l *ConsumerRateController[T]) consumeFromCache(availableTokens uint64, curState *rateControllerState[T]) []T {
	actualTakeSize := min(uint64(len(l.outputBuffer)), availableTokens)
	if actualTakeSize < availableTokens {
		// Refund the acquired tokens to reflect the reduced items.
		// This ensures that future calls are not penalized.
		curState.refund(availableTokens - actualTakeSize)
	}
	ret := l.outputBuffer[:actualTakeSize]
	l.outputBuffer = l.outputBuffer[actualTakeSize:]
	return ret
}

func (ls *rateControllerState[T]) ageGap(requiredAge time.Duration, t time.Time) time.Duration {
	return requiredAge - t.Sub(ls.startTime)
}

func (ls *rateControllerState[T]) ageGapNow(requiredAge time.Duration) time.Duration {
	return ls.ageGap(requiredAge, time.Now())
}

func (ls *rateControllerState[T]) refund(quantity uint64) {
	ls.taken.Add(-int64(quantity)) //nolint:gosec // uint64 -> int64.
}

func (ls *rateControllerState[T]) adjustAcquiredTokens(
	acquiredTokens, minItems uint64, requiredWait, softTimeout time.Duration,
) uint64 {
	// If the acquiredTokens is less or equal to minItems, we have to wait the entire duration.
	// Otherwise, we want to wait only up to SoftTimeout since the beginning of this call.
	if acquiredTokens <= minItems || requiredWait < softTimeout {
		return acquiredTokens
	}
	// Calculate how many items we overtook by limiting the wait to softTimeout.
	over := producedTokensInDuration(ls.rate, requiredWait-softTimeout)
	if over <= 0 {
		return acquiredTokens
	}
	// We deliver the difference, or minItems (the higher between them).
	if uint64(over) > acquiredTokens {
		// We handle this case differently to avoid integer overflow.
		return max(minItems, 1)
	}
	return max(acquiredTokens-uint64(over), minItems, 1)
}

// requiredDurationToProduceTokens returns the required duration to have the given number
// of tokens produced at the specified rate.
//
// producedTokens = (duration/second) * rate
// ==> (duration/second) = producedTokens / rate
// ==> duration = (producedTokens / rate) * second
// ==> duration = producedTokens * second / rate.
func requiredDurationToProduceTokens(rate, producedTokens uint64) time.Duration {
	return time.Duration(math.Floor(float64(producedTokens) * float64(time.Second) / float64(rate)))
}

// producedTokensInDuration returns the expected number of tokens produced
// after the given duration at the specified rate.
//
// producedTokens = (duration/second) * rate
// ==> producedTokens = duration * rate / second  .
func producedTokensInDuration(rate uint64, duration time.Duration) int64 {
	return int64(math.Ceil(float64(duration) * float64(rate) / float64(time.Second)))
}
