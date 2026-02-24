/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/hyperledger/fabric-x-committer/integration/runner"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

func TestRateLimit(t *testing.T) {
	t.Parallel()

	c := runner.NewRuntime(t, &runner.Config{
		BlockTimeout: 2 * time.Second,
		RateLimit: &connection.RateLimitConfig{
			RequestsPerSecond: 2,
			Burst:             1,
		},
	})

	c.Start(t, runner.FullTxPathWithQuery)

	numParallelRequests := 5

	tests := []struct {
		name             string
		endpoint         connection.WithAddress
		useRetry         bool
		requestFn        func(ctx context.Context, conn *grpc.ClientConn) error
		expectAllSucceed bool
		timeout          time.Duration
	}{
		{
			name:     "Query_WithoutRetry_ReturnsResourceExhausted",
			endpoint: c.SystemConfig.Services.Query.GrpcEndpoint,
			requestFn: func(ctx context.Context, conn *grpc.ClientConn) error {
				_, err := committerpb.NewQueryServiceClient(conn).GetTransactionStatus(ctx, &committerpb.TxStatusQuery{
					TxIds: []string{"test-tx-id"},
				})
				return err
			},
			timeout: 10 * time.Second,
		},
		{
			name:     "Query_WithRetry_AllRequestsSucceed",
			endpoint: c.SystemConfig.Services.Query.GrpcEndpoint,
			useRetry: true,
			requestFn: func(ctx context.Context, conn *grpc.ClientConn) error {
				_, err := committerpb.NewQueryServiceClient(conn).GetNamespacePolicies(ctx, &emptypb.Empty{})
				return err
			},
			expectAllSucceed: true,
			timeout:          30 * time.Second,
		},
		{
			name:     "Sidecar_WithoutRetry_ReturnsResourceExhausted",
			endpoint: c.SystemConfig.Services.Sidecar.GrpcEndpoint,
			requestFn: func(ctx context.Context, conn *grpc.ClientConn) error {
				_, err := committerpb.NewBlockQueryServiceClient(conn).GetBlockchainInfo(ctx, &emptypb.Empty{})
				return err
			},
			timeout: 10 * time.Second,
		},
		{
			name:     "Sidecar_WithRetry_ReturnsResourceExhausted",
			endpoint: c.SystemConfig.Services.Sidecar.GrpcEndpoint,
			useRetry: true,
			requestFn: func(ctx context.Context, conn *grpc.ClientConn) error {
				_, err := committerpb.NewBlockQueryServiceClient(conn).GetBlockByNumber(ctx, &committerpb.BlockNumber{})
				return err
			},
			expectAllSucceed: true,
			timeout:          30 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var conn *grpc.ClientConn
			if tt.useRetry {
				// The default retry policy includes RESOURCE_EXHAUSTED, so rate-limited
				// requests will be automatically retried and should eventually succeed.
				conn = test.NewSecuredConnection(t, tt.endpoint, c.SystemConfig.ClientTLS)
			} else {
				// Create a connection without retry policy to observe rate limiting directly.
				clientCreds, err := c.SystemConfig.ClientTLS.ClientCredentials()
				require.NoError(t, err)
				conn, err = grpc.NewClient(tt.endpoint.Address(), grpc.WithTransportCredentials(clientCreds))
				require.NoError(t, err)
				t.Cleanup(func() { _ = conn.Close() })
			}

			successCount, rateLimitedCount, otherErrorCount := makeParallelRequests(
				t, numParallelRequests, conn, tt.requestFn, tt.timeout)

			if tt.expectAllSucceed {
				require.Equal(t, int32(numParallelRequests), successCount) //nolint:gosec // int to int32
				require.Equal(t, int32(0), rateLimitedCount+otherErrorCount)
			} else {
				require.Positive(t, rateLimitedCount)
			}
		})
	}
}

func makeParallelRequests( //nolint:revive // argument-limit 5 but limit is 4
	t *testing.T,
	numParallelRequests int,
	conn *grpc.ClientConn,
	requestFn func(ctx context.Context, conn *grpc.ClientConn) error,
	timeout time.Duration,
) (success, rateLimited, otherErrors int32) {
	t.Helper()

	var successCount, rateLimitedCount, otherErrorCount atomic.Int32
	var wg sync.WaitGroup

	reqCtx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()

	for range numParallelRequests {
		wg.Go(
			func() {
				err := requestFn(reqCtx, conn)
				if err == nil {
					successCount.Add(1)
					return
				}

				st, ok := status.FromError(err)
				if ok && st.Code() == codes.ResourceExhausted {
					rateLimitedCount.Add(1)
				} else {
					otherErrorCount.Add(1)
				}
			},
		)
	}

	wg.Wait()

	return successCount.Load(), rateLimitedCount.Load(), otherErrorCount.Load()
}
