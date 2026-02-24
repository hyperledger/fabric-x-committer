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
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/hyperledger/fabric-x-committer/integration/runner"
)

func TestStreamConcurrencyLimit(t *testing.T) {
	t.Parallel()

	// The runtime's Start opens 2 long-lived streams:
	//   1. Notification stream (OpenNotificationStream)
	//   2. Deliver stream (startBlockDelivery)
	// With MaxConcurrentStreams=4, exactly 2 slots remain for the test.
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

	// Fill remaining slots with one Deliver + one Notification stream.
	// This proves both stream types share the same concurrency pool.
	// The Deliver stream uses a cancellable context so we can release it later.
	deliverClient := peer.NewDeliverClient(conn)
	notifyClient := committerpb.NewNotifierClient(conn)

	deliverCtx, deliverCancel := context.WithCancel(t.Context())
	_, err = deliverClient.Deliver(deliverCtx)
	require.NoError(t, err)

	_, err = notifyClient.OpenNotificationStream(t.Context())
	require.NoError(t, err)

	// All 4 slots are now occupied (2 from Start + 1 Deliver + 1 Notification).
	// The next stream of either type should be rejected.
	_, err = deliverClient.Deliver(t.Context())
	requireResourceExhausted(t, err)

	_, err = notifyClient.OpenNotificationStream(t.Context())
	requireResourceExhausted(t, err)

	// Cancel the Deliver stream to release one semaphore slot.
	// The server-side handler must return before the semaphore is released,
	// so we poll with require.Eventually to tolerate the cleanup delay.
	deliverCancel()

	require.Eventually(t, func() bool {
		_, err = notifyClient.OpenNotificationStream(t.Context())
		return err == nil
	}, 5*time.Second, 100*time.Millisecond, "new stream should succeed after releasing a slot")
}

func requireResourceExhausted(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	require.Equal(t, codes.ResourceExhausted, st.Code())
}
