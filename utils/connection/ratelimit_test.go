/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package connection

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestNewRateLimiter(t *testing.T) {
	t.Parallel()

	t.Run("nil config returns nil limiter", func(t *testing.T) {
		t.Parallel()
		limiter := NewRateLimiter(nil)
		require.Nil(t, limiter)
	})

	t.Run("zero rate returns nil limiter", func(t *testing.T) {
		t.Parallel()
		limiter := NewRateLimiter(&RateLimitConfig{RequestsPerSecond: 0})
		require.Nil(t, limiter)
	})

	t.Run("negative rate returns nil limiter", func(t *testing.T) {
		t.Parallel()
		limiter := NewRateLimiter(&RateLimitConfig{RequestsPerSecond: -1})
		require.Nil(t, limiter)
	})

	t.Run("positive rate returns valid limiter with configured burst", func(t *testing.T) {
		t.Parallel()
		limiter := NewRateLimiter(&RateLimitConfig{RequestsPerSecond: 100, Burst: 50})
		require.NotNil(t, limiter)
		require.Equal(t, int(100), int(limiter.Limit()))
		require.Equal(t, 50, limiter.Burst())
	})
}

func TestRateLimitConfigValidate(t *testing.T) {
	t.Parallel()

	t.Run("nil config is valid", func(t *testing.T) {
		t.Parallel()
		var config *RateLimitConfig
		require.NoError(t, config.Validate())
	})

	t.Run("disabled rate limiting is valid", func(t *testing.T) {
		t.Parallel()
		config := &RateLimitConfig{RequestsPerSecond: 0, Burst: 100}
		require.NoError(t, config.Validate())
	})

	t.Run("burst less than requests-per-second is valid", func(t *testing.T) {
		t.Parallel()
		config := &RateLimitConfig{RequestsPerSecond: 1000, Burst: 200}
		require.NoError(t, config.Validate())
	})

	t.Run("burst equal to requests-per-second is valid", func(t *testing.T) {
		t.Parallel()
		config := &RateLimitConfig{RequestsPerSecond: 200, Burst: 200}
		require.NoError(t, config.Validate())
	})

	t.Run("burst zero is invalid when rate limiting is enabled", func(t *testing.T) {
		t.Parallel()
		config := &RateLimitConfig{RequestsPerSecond: 100, Burst: 0}
		err := config.Validate()
		require.Error(t, err)
		require.ErrorContains(t, err, "burst must be greater than 0")
	})

	t.Run("burst greater than requests-per-second is invalid", func(t *testing.T) {
		t.Parallel()
		config := &RateLimitConfig{RequestsPerSecond: 100, Burst: 200}
		err := config.Validate()
		require.Error(t, err)
		require.ErrorContains(t, err, "burst (200) must be less than or equal to requests-per-second")
	})
}

func TestRateLimitInterceptor(t *testing.T) {
	t.Parallel()

	const s = "success"
	handler := func(_ context.Context, _ any) (any, error) {
		return s, nil
	}

	t.Run("nil limiter allows all requests", func(t *testing.T) {
		t.Parallel()
		interceptor := RateLimitInterceptor(nil)
		for range 10 {
			resp, err := interceptor(t.Context(), nil, &grpc.UnaryServerInfo{}, handler)
			require.NoError(t, err)
			require.Equal(t, s, resp)
		}
	})

	t.Run("rate limited returns ResourceExhausted", func(t *testing.T) {
		t.Parallel()
		limiter := rate.NewLimiter(rate.Limit(1), 1)
		interceptor := RateLimitInterceptor(limiter)

		// First request should succeed (uses the burst)
		resp, err := interceptor(t.Context(), nil, &grpc.UnaryServerInfo{}, handler)
		require.NoError(t, err)
		require.Equal(t, "success", resp)

		// Second immediate request should be rate limited
		_, err = interceptor(t.Context(), nil, &grpc.UnaryServerInfo{}, handler)
		require.Error(t, err)

		st, ok := status.FromError(err)
		require.True(t, ok)
		require.Equal(t, codes.ResourceExhausted, st.Code())
		require.Equal(t, "rate limit exceeded", st.Message())
	})

	t.Run("handler errors are passed through", func(t *testing.T) {
		t.Parallel()
		limiter := rate.NewLimiter(rate.Inf, 1)
		interceptor := RateLimitInterceptor(limiter)
		expectedErr := status.Error(codes.Internal, "handler error")
		handlerRetErr := func(_ context.Context, _ any) (any, error) {
			return nil, expectedErr
		}

		resp, err := interceptor(t.Context(), nil, &grpc.UnaryServerInfo{}, handlerRetErr)
		require.Nil(t, resp)
		require.Equal(t, expectedErr, err)
	})

	t.Run("concurrent requests exhaust burst and get rate limited", func(t *testing.T) {
		t.Parallel()
		limiter := rate.NewLimiter(rate.Limit(5), 5)
		interceptor := RateLimitInterceptor(limiter)

		successCount := 0
		rateLimitedCount := 0

		// Make 10 immediate requests - first 5 should succeed, rest should be rate limited
		for range 10 {
			_, err := interceptor(t.Context(), nil, &grpc.UnaryServerInfo{}, handler)
			if err != nil {
				st, ok := status.FromError(err)
				if ok && st.Code() == codes.ResourceExhausted {
					rateLimitedCount++
				}
			} else {
				successCount++
			}
		}

		require.Equal(t, 5, successCount)
		require.Equal(t, 5, rateLimitedCount)
	})
}
