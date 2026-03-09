/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package connection

import (
	"context"

	"golang.org/x/sync/semaphore"
	"golang.org/x/time/rate"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// NewRateLimiter creates a rate limiter based on the configuration.
func NewRateLimiter(config *RateLimitConfig) *rate.Limiter {
	if config == nil || config.RequestsPerSecond <= 0 {
		return nil
	}
	return rate.NewLimiter(rate.Limit(config.RequestsPerSecond), config.Burst)
}

// RateLimitInterceptor returns a UnaryServerInterceptor that implements
// rate limiting using a token bucket algorithm.
func RateLimitInterceptor(limiter *rate.Limiter) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		if !limiter.Allow() {
			logger.Warnf("Rate limit exceeded, rejecting request for method: %s", info.FullMethod)
			return nil, status.Error(codes.ResourceExhausted, "rate limit exceeded")
		}
		return handler(ctx, req)
	}
}

// NewConcurrencyLimit creates a weighted semaphore for limiting concurrent streams.
func NewConcurrencyLimit(maxConcurrentStreams int) *semaphore.Weighted {
	if maxConcurrentStreams <= 0 {
		return nil
	}
	return semaphore.NewWeighted(int64(maxConcurrentStreams))
}

// StreamConcurrencyInterceptor returns a gRPC StreamServerInterceptor that limits the number of
// concurrently active streaming RPCs using a weighted semaphore.
func StreamConcurrencyInterceptor(sem *semaphore.Weighted) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if !sem.TryAcquire(1) {
			return status.Error(codes.ResourceExhausted, "max concurrent streams limit reached")
		}
		defer sem.Release(1)
		return handler(srv, ss)
	}
}
