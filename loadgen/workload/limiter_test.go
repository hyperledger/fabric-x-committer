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
	name     string
	rate     uint64
	minBatch uint64
	maxWait  time.Duration
	takeSize uint64
	workers  int
}

func TestLimiterTargetRate(t *testing.T) {
	t.Parallel()
	for _, tc := range allRateTestCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expectedSeconds := 5
			producedTotal := expectedSeconds * int(tc.rate) //nolint:gosec // uint64 -> int.
			producesPerGen := int64(producedTotal / tc.workers)

			l := NewRateLimiter(&LimiterConfig{
				Rate:         tc.rate,
				MinBatchSize: tc.minBatch,
				MaxBatchWait: tc.maxWait,
			})

			ctx, cancel := context.WithTimeout(t.Context(), time.Second*time.Duration(expectedSeconds*2))
			t.Cleanup(cancel)
			wg := sync.WaitGroup{}
			wg.Add(tc.workers)
			start := time.Now()
			for range tc.workers {
				go func() {
					defer wg.Done()
					taken := int64(0)
					for taken < producesPerGen && ctx.Err() == nil {
						//nolint:gosec // uint64 -> int.
						request := min(int64(tc.takeSize), producesPerGen-taken)
						curTake := l.Take(ctx, request)
						//nolint:gosec // uint64 -> int64.
						assert.GreaterOrEqual(t, curTake, min(request, int64(tc.minBatch)))
						taken += curTake
					}
				}()
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
	conf := &LimiterConfig{
		Rate:         100,
		MinBatchSize: 1,
		MaxBatchWait: 100 * time.Millisecond,
	}
	l := NewRateLimiter(conf)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	t.Cleanup(cancel)
	for range 100 {
		start := time.Now()
		taken := l.Take(ctx, 1_000)
		duration := time.Since(start)
		assert.Positive(t, taken)
		assert.Less(t, duration, conf.MaxBatchWait+20*time.Millisecond)
	}
}

func TestLimiterMinBatch(t *testing.T) {
	t.Parallel()
	conf := &LimiterConfig{
		Rate:         100,
		MinBatchSize: 10,
		MaxBatchWait: 5 * time.Millisecond,
	}
	l := NewRateLimiter(conf)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	t.Cleanup(cancel)
	for range 100 {
		start := time.Now()
		taken := l.Take(ctx, 1_000)
		duration := time.Since(start)
		assert.EqualValues(t, conf.MinBatchSize, taken)
		assert.Less(t, duration, 120*time.Millisecond)
	}
}

func allRateTestCases() []rateTestCase {
	var rateTestCases []rateTestCase
	for _, rate := range []uint64{10, 1_000, 10_000} {
		for _, minBatch := range []uint64{1, 100} {
			for _, maxWait := range []time.Duration{0, 50 * time.Millisecond, time.Second} {
				for _, takeSize := range []uint64{10, 1_000, 10_000} {
					for _, workers := range []int{1, 5} {
						name := fmt.Sprintf("rate=%d,batch=%d,wait=%v,take=%d,workers=%d",
							rate, minBatch, maxWait, takeSize, workers)
						rateTestCases = append(rateTestCases, rateTestCase{
							name:     name,
							rate:     rate,
							minBatch: minBatch,
							maxWait:  maxWait,
							takeSize: takeSize,
							workers:  workers,
						})
					}
				}
			}
		}
	}
	return rateTestCases
}
