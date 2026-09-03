/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"context"
	"testing"
	"time"

	"github.com/hyperledger/fabric-protos-go-apiv2/peer"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/hyperledger/fabric-x-committer/integration/runner"
)

func TestStreamConcurrencyLimit(t *testing.T) {
	t.Parallel()

	// The runtime's Start opens 3 long-lived streams:
	//   1. Deliver stream (startBlockDelivery)
	//   2. Notification stream (OpenNotificationStream)
	//   3. Notification block stream (StreamBlocks)
	// With MaxConcurrentStreams=4, exactly 1 slot remains for the test.
	const maxStreams = 4
	c := runner.NewRuntime(t, &runner.Config{
		BlockTimeout:         2 * time.Second,
		MaxConcurrentStreams: maxStreams,
	})
	c.Start(t, runner.FullTxPath)

	// Create a raw gRPC connection to the sidecar without retry policy.
	// The default retry policy includes RESOURCE_EXHAUSTED, which would
	// mask the concurrency limit behavior by retrying rejected streams.
	sidecarEndpoint := c.SystemConfig.Services.Sidecar.GrpcEndpoint
	clientCreds, err := c.SystemConfig.ClientTLS.ClientCredentials()
	require.NoError(t, err)
	conn, err := grpc.NewClient(
		sidecarEndpoint.Address(),
		grpc.WithTransportCredentials(clientCreds),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	// Fill the remaining slot with one Deliver stream.
	// The Deliver stream uses a cancellable context so we can release it later.
	deliverClient := peer.NewDeliverClient(conn)
	notifyClient := committerpb.NewSidecarServiceClient(conn)

	deliverCtx, deliverCancel := context.WithCancel(t.Context())
	_, err = deliverClient.Deliver(deliverCtx)
	require.NoError(t, err)

	// Wait for the server to start the stream handler above and acquire
	// its semaphore slot. The server processes new streams asynchronously
	// (goroutine per stream), so without this the interceptor may not have
	// called TryAcquire yet when we check for rejection below.
	time.Sleep(2 * time.Second)

	// All 4 slots are now occupied (3 from Start + 1 Deliver).
	// The next stream of any type should be rejected.
	//
	// For bidirectional streaming RPCs, the server-side interceptor error
	// (ResourceExhausted) is NOT returned from the initial stream creation
	// call. gRPC Go's NewStream sends HTTP/2 HEADERS and returns immediately;
	// the server processes the stream asynchronously. The error only surfaces
	// via Recv() when the server closes the rejected stream with a status.
	rejectedDeliver, err := deliverClient.Deliver(t.Context())
	if err == nil {
		_, err = rejectedDeliver.Recv()
	}
	requireResourceExhausted(t, err)

	rejectedNotify, err := notifyClient.OpenNotificationStream(t.Context())
	if err == nil {
		_, err = rejectedNotify.Recv()
	}
	requireResourceExhausted(t, err)

	rejectedStreamBlocks, err := notifyClient.StreamBlocks(t.Context(), nil)
	if err == nil {
		_, err = rejectedStreamBlocks.Recv()
	}
	requireResourceExhausted(t, err)

	// Cancel the Deliver stream to release one semaphore slot.
	// The server-side handler must return before the semaphore is released,
	// so we poll with require.Eventually to tolerate the cleanup delay.
	deliverCancel()

	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
		defer cancel()
		stream, streamErr := notifyClient.OpenNotificationStream(ctx)
		require.NoError(ct, streamErr)
		_, recvErr := stream.Recv()
		// Accepted: handler runs, Recv blocks until context timeout (DeadlineExceeded)
		// Rejected: interceptor returns ResourceExhausted, Recv gets it immediately
		require.NotEqual(ct, codes.ResourceExhausted, status.Code(recvErr))
	}, 5*time.Second, 100*time.Millisecond, "new stream should succeed after releasing a slot")
}

func requireResourceExhausted(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	require.Equal(t, codes.ResourceExhausted, st.Code())
}
