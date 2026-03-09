/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package connection

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type mockServerStream struct {
	grpc.ServerStream
}

func (*mockServerStream) Context() context.Context {
	return context.Background()
}

func TestConcurrencyLimitInterceptors(t *testing.T) {
	t.Parallel()

	const (
		limit = 5
		total = 20
	)

	tests := []struct {
		name    string
		invoker func(admitted *atomic.Int32, proceed chan any) func() error
	}{
		{
			name: "rate limit interceptor",
			invoker: func(admitted *atomic.Int32, _ chan any) func() error {
				interceptor := RateLimitInterceptor(NewRateLimiter(&RateLimitConfig{
					RequestsPerSecond: 10, Burst: limit,
				}))
				return func() error {
					_, err := interceptor(context.Background(), nil, &grpc.UnaryServerInfo{},
						func(_ context.Context, _ any) (any, error) {
							admitted.Add(1)
							return "ok", nil
						})
					return err
				}
			},
		},
		{
			name: "stream concurrency interceptor",
			invoker: func(admitted *atomic.Int32, proceed chan any) func() error {
				interceptor := StreamConcurrencyInterceptor(NewConcurrencyLimit(limit))
				return func() error {
					return interceptor(nil, &mockServerStream{}, nil, func(_ any, _ grpc.ServerStream) error {
						admitted.Add(1)
						<-proceed
						return nil
					})
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var admitted atomic.Int32
			var rejected atomic.Int32
			var wg sync.WaitGroup

			proceed := make(chan any)
			invoke := tt.invoker(&admitted, proceed)

			for range total {
				wg.Go(func() {
					if err := invoke(); err != nil {
						assert.Equal(t, codes.ResourceExhausted, status.Code(err))
						rejected.Add(1)
					}
				})
			}

			require.Eventually(t, func() bool {
				return admitted.Load()+rejected.Load() == total
			}, 5*time.Second, 10*time.Millisecond)

			require.Equal(t, int32(limit), admitted.Load())
			require.Equal(t, int32(total-limit), rejected.Load())

			close(proceed)
			wg.Wait()
		})
	}
}

func TestNewRateLimiter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		config        *RateLimitConfig
		expectNil     bool
		expectedRate  int
		expectedBurst int
	}{
		{
			name:      "nil config returns nil limiter",
			config:    nil,
			expectNil: true,
		},
		{
			name:      "zero rate returns nil limiter",
			config:    &RateLimitConfig{RequestsPerSecond: 0},
			expectNil: true,
		},
		{
			name:      "negative rate returns nil limiter",
			config:    &RateLimitConfig{RequestsPerSecond: -1},
			expectNil: true,
		},
		{
			name:          "positive rate returns valid limiter",
			config:        &RateLimitConfig{RequestsPerSecond: 100, Burst: 50},
			expectNil:     false,
			expectedRate:  100,
			expectedBurst: 50,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			limiter := NewRateLimiter(tt.config)
			if tt.expectNil {
				require.Nil(t, limiter)
			} else {
				require.NotNil(t, limiter)
				require.Equal(t, tt.expectedRate, int(limiter.Limit()))
				require.Equal(t, tt.expectedBurst, limiter.Burst())
			}
		})
	}
}

func TestNewConcurrencyLimit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		limit     int
		expectNil bool
	}{
		{
			name:      "zero limit returns nil",
			limit:     0,
			expectNil: true,
		},
		{
			name:      "negative limit returns nil",
			limit:     -1,
			expectNil: true,
		},
		{
			name:      "positive limit returns semaphore",
			limit:     10,
			expectNil: false,
		},
		{
			name:      "limit of 1 returns semaphore",
			limit:     1,
			expectNil: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sem := NewConcurrencyLimit(tt.limit)
			if tt.expectNil {
				require.Nil(t, sem)
			} else {
				require.NotNil(t, sem)
				// Verify we can acquire up to the limit
				require.True(t, sem.TryAcquire(int64(tt.limit)))
				// Verify we can't acquire more than the limit
				require.False(t, sem.TryAcquire(1))
				sem.Release(int64(tt.limit))
			}
		})
	}
}
