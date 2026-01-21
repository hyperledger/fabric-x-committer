/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package workload

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type rateTestCase struct {
	name        string
	rate        uint64
	requestSize uint64
	minBatch    uint64
	maxWait     time.Duration
	workers     uint64
}

func TestLimiterTargetRate(t *testing.T) {
	t.Parallel()
	for _, tc := range allRateTestCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expectedSeconds := uint64(5)
			producedTotal := expectedSeconds * tc.rate
			producesPerGen := producedTotal / tc.workers

			l := NewConsumerRateController(tc.rate, runProducer(t))

			//nolint:gosec // uint64 -> int64 in duration.
			ctx, cancel := context.WithTimeout(t.Context(), time.Second*time.Duration(expectedSeconds*2))
			t.Cleanup(cancel)
			wg := sync.WaitGroup{}
			start := time.Now()
			for range tc.workers {
				wg.Go(func() {
					g := l.InstantiateWorker()
					taken := uint64(0)
					p := ConsumeParameters{
						MinItems:    tc.minBatch,
						SoftTimeout: tc.maxWait,
					}
					for taken < producesPerGen && ctx.Err() == nil {
						p.RequestedItems = min(tc.requestSize, producesPerGen-taken)
						curTake := g.Consume(ctx, p)
						require.NotNil(t, curTake)
						assert.GreaterOrEqual(t, uint64(len(curTake)), min(p.RequestedItems, tc.minBatch))
						taken += uint64(len(curTake))
					}
				})
			}
			wg.Wait()
			duration := time.Since(start)
			t.Logf("duration: %s", duration)
			require.InDelta(t, float64(expectedSeconds), duration.Seconds(), 0.2*float64(expectedSeconds))
		})
	}
}

func TestLimiterMaxWait(t *testing.T) {
	t.Parallel()
	l := NewConsumerRateController(100, runProducer(t))
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(cancel)
	p := ConsumeParameters{
		RequestedItems: 1_000,
		MinItems:       1,
		SoftTimeout:    time.Second,
	}
	for i := range 10 {
		start := time.Now()
		taken := l.Consume(ctx, p)
		duration := time.Since(start)
		assert.NotNil(t, taken)
		assert.Lessf(t, duration, p.SoftTimeout+200*time.Millisecond, "Iteration: %d", i)
		assert.GreaterOrEqualf(t, len(taken), 80, "Iteration: %d", i)
	}
}

func TestLimiterMinBatch(t *testing.T) {
	t.Parallel()
	l := NewConsumerRateController(100, runProducer(t))
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(cancel)
	p := ConsumeParameters{
		RequestedItems: 1_000,
		MinItems:       100,
		SoftTimeout:    time.Millisecond,
	}
	for i := range 10 {
		start := time.Now()
		taken := l.Consume(ctx, p)
		duration := time.Since(start)
		assert.NotNil(t, taken)
		assert.Len(t, taken, int(p.MinItems), "Iteration: %d", i) //nolint:gosec // uint64 -> int.
		assert.Less(t, duration, time.Second+200*time.Millisecond, "Iteration: %d", i)
	}
}

func allRateTestCases() []rateTestCase {
	rateTestCases := make([]rateTestCase, 0, 3*2*3*3*2)
	for _, rate := range []uint64{10, 1_000, 10_000} {
		for _, minBatch := range []uint64{1, 100} {
			for _, maxWait := range []time.Duration{0, 50 * time.Millisecond, time.Second} {
				for _, requestSize := range []uint64{10, 1_000, 10_000} {
					for _, workers := range []uint64{1, 5} {
						name := fmt.Sprintf("rate=%d,batch=%d,wait=%v,request=%d,workers=%d",
							rate, minBatch, maxWait, requestSize, workers)
						rateTestCases = append(rateTestCases, rateTestCase{
							name:        name,
							rate:        rate,
							minBatch:    minBatch,
							maxWait:     maxWait,
							requestSize: requestSize,
							workers:     workers,
						})
					}
				}
			}
		}
	}
	return rateTestCases
}

func runProducer(t *testing.T) chan []any {
	t.Helper()
	queue := make(chan []any, 1)
	queue <- make([]any, 100_000)
	producerWg := &sync.WaitGroup{}
	t.Cleanup(producerWg.Wait)
	ctx := t.Context()
	producerWg.Go(func() {
		for ctx.Err() == nil {
			select {
			case queue <- make([]any, 100_000):
			case <-ctx.Done():
				return
			}
		}
	})
	return queue
}
