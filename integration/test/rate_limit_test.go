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

	"github.com/hyperledger/fabric-x-committer/integration/runner"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

func TestRateLimit(t *testing.T) {
	t.Parallel()

	c := runner.NewRuntime(t, &runner.Config{
		NumVerifiers: 1,
		NumVCService: 1,
		BlockTimeout: 2 * time.Second,
		RateLimit: &connection.RateLimitConfig{
			RequestsPerSecond: 2,
			Burst:             1,
		},
	})

	c.Start(t, runner.FullTxPathWithQuery)

	numParallelRequests := 5

	t.Run("WithoutRetry_ReturnsResourceExhausted", func(t *testing.T) {
		t.Parallel()
		// Create a connection without retry policy to observe rate limiting behavior.
		// The default test connection has a retry policy that includes RESOURCE_EXHAUSTED,
		// which would mask the rate limit behavior by automatically retrying failed requests.
		clientCreds, err := c.SystemConfig.ClientTLS.ClientCredentials()
		require.NoError(t, err)
		conn, err := grpc.NewClient(
			c.SystemConfig.Endpoints.Query.Server.Address(),
			grpc.WithTransportCredentials(clientCreds),
		)
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })

		_, rateLimitedCount, _ := makeParallelRequests(t, numParallelRequests, conn, 10*time.Second)
		require.Positive(t, rateLimitedCount)
	})

	t.Run("WithRetry_AllRequestsSucceed", func(t *testing.T) {
		t.Parallel()
		// Create a connection with the default retry policy.
		// The retry policy includes RESOURCE_EXHAUSTED, so rate-limited requests
		// will be automatically retried and should eventually succeed.
		conn := test.NewSecuredConnection(t, c.SystemConfig.Endpoints.Query.Server, c.SystemConfig.ClientTLS)

		successCount, rateLimitedCount, otherErrorCount := makeParallelRequests(
			t, numParallelRequests, conn, 30*time.Second)

		// With retry policy, all requests should eventually succeed
		require.Equal(t, int32(numParallelRequests), successCount) //nolint:gosec // int to int32
		require.Equal(t, int32(0), rateLimitedCount+otherErrorCount)
	})
}

func makeParallelRequests(t *testing.T, numParallelRequests int, conn *grpc.ClientConn, timeout time.Duration) (
	success, rateLimited, otherErrors int32,
) {
	t.Helper()

	client := committerpb.NewQueryServiceClient(conn)

	var successCount, rateLimitedCount, otherErrorCount atomic.Int32
	var wg sync.WaitGroup

	reqCtx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()

	for range numParallelRequests {
		wg.Go(
			func() {
				_, err := client.GetTransactionStatus(reqCtx, &committerpb.TxStatusQuery{
					TxIds: []string{"test-tx-id"},
				})
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
