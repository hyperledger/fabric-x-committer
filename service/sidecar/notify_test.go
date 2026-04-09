/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package sidecar

import (
	"context"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/hyperledger/fabric-lib-go/common/flogging"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

func BenchmarkNotifier(b *testing.B) {
	flogging.ActivateSpec("fatal")
	txIDs := make([]string, b.N)
	for i := range txIDs {
		txIDs[i] = fmt.Sprintf("%064d", i)
	}

	batchSize := 4096
	requests := make([]*committerpb.NotificationRequest, 0, (b.N/batchSize)+1)
	statuses := make([][]*committerpb.TxStatus, 0, (b.N/batchSize)+1)
	requestTxIDs := txIDs
	for len(requestTxIDs) > 0 {
		sz := min(batchSize, len(requestTxIDs))
		requests = append(requests, &committerpb.NotificationRequest{
			TxStatusRequest: &committerpb.TxIDsBatch{
				TxIds: txIDs[:sz],
			},
			Timeout: durationpb.New(1 * time.Hour),
		})
		requestTxIDs = requestTxIDs[sz:]
	}
	statusTxIDs := txIDs
	rand.Shuffle(len(statusTxIDs), func(i, j int) {
		statusTxIDs[i], statusTxIDs[j] = statusTxIDs[j], statusTxIDs[i]
	})
	for len(statusTxIDs) > 0 {
		sz := min(batchSize, len(statusTxIDs))
		status := make([]*committerpb.TxStatus, sz)
		for i, txID := range statusTxIDs[:sz] {
			status[i] = &committerpb.TxStatus{Ref: committerpb.NewTxRef(txID, 0, 0)}
		}
		statuses = append(statuses, status)
		statusTxIDs = statusTxIDs[sz:]
	}

	env := newNotifierTestEnv(b, 5)
	q := env.responseQueues[0]

	// We benchmark a full cycle, adding TX IDs, removing them, and getting the notifications.
	b.ResetTimer()
	for _, r := range requests {
		env.requestQueue.Write(&notificationRequest{
			request:          r,
			streamEventQueue: q,
		})
	}

	// Ensures switching to the notifier worker to handle the request, before submitting the statuses.
	for len(env.n.requestQueue) > 0 {
		time.Sleep(10 * time.Millisecond)
	}

	for _, s := range statuses {
		env.statusQueue.Write(s)
	}

	expectedCount := len(txIDs)
	notifiedCount := 0
	for notifiedCount < expectedCount {
		res, ok := q.ReadWithTimeout(5 * time.Minute)
		if !ok {
			b.Fatal("expected notification")
		}
		notifiedCount += len(res.TxStatusEvents)
	}
	b.StopTimer()
}

type notifierTestEnv struct {
	n              *notifier
	metrics        *perfMetrics
	requestQueue   channel.Writer[*notificationRequest]
	statusQueue    channel.Writer[[]*committerpb.TxStatus]
	responseQueues []channel.ReaderWriter[*committerpb.NotificationResponse]
}

func TestNotifierDirect(t *testing.T) {
	t.Parallel()
	env := newNotifierTestEnv(t, 5)
	m := env.metrics

	// Initial metrics should be zero
	test.RequireIntMetricValue(t, 0, m.notifierPendingTxIDs)
	test.RequireIntMetricValue(t, 0, m.notifierUniquePendingTxIDs)
	test.RequireIntMetricValue(t, 0, m.notifierTxIDsStatusDeliveries)
	test.RequireIntMetricValue(t, 0, m.notifierTxIDsTimeoutDeliveries)

	t.Log("Submitting requests")
	for _, q := range env.responseQueues {
		env.requestQueue.Write(&notificationRequest{
			request: &committerpb.NotificationRequest{
				TxStatusRequest: &committerpb.TxIDsBatch{
					TxIds: []string{"1", "2", "3", "4", "5", "5", "5", "5", "6"},
				},
				Timeout: durationpb.New(5 * time.Minute),
			},
			streamEventQueue: q,
		})
	}

	t.Log("No events - not expecting notifications")
	time.Sleep(3 * time.Second)
	for _, q := range env.responseQueues {
		_, ok := q.ReadWithTimeout(10 * time.Millisecond)
		require.False(t, ok, "should not receive notification")
	}

	// Verify pending txIDs: 5 queues * 6 unique txIDs = 30 pending subscriptions
	// Unique pending: only 6 unique txIDs across all queues (first request adds 6, rest add 0)
	test.RequireIntMetricValue(t, 30, m.notifierPendingTxIDs)
	test.RequireIntMetricValue(t, 6, m.notifierUniquePendingTxIDs)

	t.Log("Submitting events - expecting notifications")
	expected := []*committerpb.TxStatus{
		{Ref: committerpb.NewTxRef("1", 1, 1)},
		{Ref: committerpb.NewTxRef("2", 5, 1)},
	}
	env.statusQueue.Write(expected)
	for _, q := range env.responseQueues {
		res, ok := q.ReadWithTimeout(10 * time.Second)
		require.True(t, ok)
		require.NotNil(t, res)
		require.Empty(t, res.TimeoutTxIds)
		test.RequireProtoElementsMatch(t, expected, res.TxStatusEvents)
	}

	// Verify metrics: 2 statuses delivered to 5 queues = 10 deliveries, pending 30-10=20
	// Unique pending: 6-2=4 (txIDs "1" and "2" removed from map)
	test.RequireIntMetricValue(t, 20, m.notifierPendingTxIDs)
	test.RequireIntMetricValue(t, 4, m.notifierUniquePendingTxIDs)
	test.RequireIntMetricValue(t, 10, m.notifierTxIDsStatusDeliveries)

	t.Log("Not expecting more notifications")
	time.Sleep(3 * time.Second)
	for _, q := range env.responseQueues {
		_, ok := q.ReadWithTimeout(10 * time.Millisecond)
		require.False(t, ok, "should not receive notification")
	}

	t.Log("Submitting irrelevant events - not expecting notifications")
	env.statusQueue.Write([]*committerpb.TxStatus{
		{Ref: committerpb.NewTxRef("100", 1, 1)},
		{Ref: committerpb.NewTxRef("200", 5, 1)},
	})
	time.Sleep(3 * time.Second)
	for _, q := range env.responseQueues {
		_, ok := q.ReadWithTimeout(10 * time.Millisecond)
		require.False(t, ok, "should not receive notification")
	}

	t.Log("Submitting more events - expecting notifications")
	expected = []*committerpb.TxStatus{
		{Ref: committerpb.NewTxRef("3", 2, 5)},
		{Ref: committerpb.NewTxRef("4", 3, 10)},
	}
	env.statusQueue.Write(expected)
	for _, q := range env.responseQueues {
		res, ok := q.Read()
		require.True(t, ok)
		require.NotNil(t, res)
		require.Empty(t, res.TimeoutTxIds)
		test.RequireProtoElementsMatch(t, expected, res.TxStatusEvents)
	}

	// Verify metrics: 2 more statuses to 5 queues = 10 more deliveries, pending 20-10=10
	// Unique pending: 4-2=2 (txIDs "3" and "4" removed from map, "5" and "6" remain)
	test.RequireIntMetricValue(t, 10, m.notifierPendingTxIDs)
	test.RequireIntMetricValue(t, 2, m.notifierUniquePendingTxIDs)
	test.RequireIntMetricValue(t, 20, m.notifierTxIDsStatusDeliveries)

	t.Log("Submitting requests with short timeout - expecting notifications")
	timeoutIDs := []string{"5", "6", "7", "8"}
	for _, q := range env.responseQueues {
		env.requestQueue.Write(&notificationRequest{
			request: &committerpb.NotificationRequest{
				TxStatusRequest: &committerpb.TxIDsBatch{
					TxIds: timeoutIDs,
				},
				Timeout: durationpb.New(1 * time.Millisecond),
			},
			streamEventQueue: q,
		})
	}
	for _, q := range env.responseQueues {
		res, ok := q.ReadWithTimeout(10 * time.Second)
		require.True(t, ok)
		require.NotNil(t, res)
		require.Empty(t, res.TxStatusEvents)
		require.ElementsMatch(t, timeoutIDs, res.TimeoutTxIds)
	}

	// Verify timeout metrics: 4 txIDs timed out for 5 queues = 20 timeout deliveries
	// pending was 10 + 5*4 = 30, now 30-20 = 10 (original "5" and "6" still pending)
	// Unique pending: was 2+2=4 (first timeout req added "7", "8"), now 4-2=2 ("7", "8" fully removed)
	test.RequireIntMetricValue(t, 10, m.notifierPendingTxIDs)
	test.RequireIntMetricValue(t, 2, m.notifierUniquePendingTxIDs)
	test.RequireIntMetricValue(t, 20, m.notifierTxIDsTimeoutDeliveries)

	t.Log("Submitting event with duplicate request - expecting single notification")
	expected = []*committerpb.TxStatus{
		{Ref: committerpb.NewTxRef("5", 3, 0)},
	}
	env.statusQueue.Write(expected)
	for _, q := range env.responseQueues {
		res, ok := q.Read()
		require.True(t, ok)
		require.NotNil(t, res)
		require.Empty(t, res.TimeoutTxIds)
		test.RequireProtoElementsMatch(t, expected, res.TxStatusEvents)
	}

	// Verify metrics: 1 status to 5 queues = 5 deliveries, pending 10-5=5
	// Unique pending: 2-1=1 (txID "5" removed, "6" remains)
	test.RequireIntMetricValue(t, 5, m.notifierPendingTxIDs)
	test.RequireIntMetricValue(t, 1, m.notifierUniquePendingTxIDs)
	test.RequireIntMetricValue(t, 25, m.notifierTxIDsStatusDeliveries)

	t.Log("Submitting duplicated event - expecting single notification")
	expected = []*committerpb.TxStatus{
		{Ref: committerpb.NewTxRef("6", 4, 0)},
		{Ref: committerpb.NewTxRef("6", 4, 1)},
		{Ref: committerpb.NewTxRef("6", 4, 2)},
	}
	env.statusQueue.Write(expected)
	for _, q := range env.responseQueues {
		res, ok := q.Read()
		require.True(t, ok)
		require.NotNil(t, res)
		require.Empty(t, res.TimeoutTxIds)
		test.RequireProtoElementsMatch(t, expected[:1], res.TxStatusEvents)
	}

	// Final metrics: 1 more status to 5 queues = 5 more deliveries, pending 5-5=0
	// Unique pending: 1-1=0 (txID "6" removed, all done)
	test.RequireIntMetricValue(t, 0, m.notifierPendingTxIDs)
	test.RequireIntMetricValue(t, 0, m.notifierUniquePendingTxIDs)
	test.RequireIntMetricValue(t, 30, m.notifierTxIDsStatusDeliveries)

	t.Log("Submitting request exceeding per-request limit - expecting rejection")
	perRequestLimit := env.n.maxTxIDsPerRequest
	overLimitTxIDs := make([]string, perRequestLimit+1)
	for i := range overLimitTxIDs {
		overLimitTxIDs[i] = fmt.Sprintf("over-%d", i)
	}
	for _, q := range env.responseQueues {
		env.requestQueue.Write(&notificationRequest{
			request: &committerpb.NotificationRequest{
				TxStatusRequest: &committerpb.TxIDsBatch{
					TxIds: overLimitTxIDs,
				},
				Timeout: durationpb.New(5 * time.Minute),
			},
			streamEventQueue: q,
		})
	}
	for _, q := range env.responseQueues {
		res, ok := q.ReadWithTimeout(10 * time.Second)
		require.True(t, ok)
		require.NotNil(t, res.RejectedTxIds)
		require.ElementsMatch(t, overLimitTxIDs, res.RejectedTxIds.TxIds)
		require.Contains(t, res.RejectedTxIds.Reason, fmt.Sprintf("max allowed is %d", perRequestLimit))
	}
}

func TestNotifierStream(t *testing.T) {
	t.Parallel()
	env := newNotifierTestEnv(t, 5)
	m := env.metrics
	config := test.NewLocalHostServer(test.InsecureTLSConfig)
	test.RunGrpcServerForTest(t.Context(), t, config, func(server *grpc.Server) {
		committerpb.RegisterNotifierServer(server, env.n)
	})
	endpoint := &config.Endpoint
	conn := test.NewInsecureConnection(t, endpoint)
	client := committerpb.NewNotifierClient(conn)

	stream, err := client.OpenNotificationStream(t.Context())
	require.NoError(t, err)

	// Verify active stream metric
	test.EventuallyIntMetric(t, 1, m.notifierActiveStreams, 5*time.Second, 100*time.Millisecond)

	t.Log("Submitting requests")
	err = stream.Send(&committerpb.NotificationRequest{
		TxStatusRequest: &committerpb.TxIDsBatch{
			TxIds: []string{"1", "2", "3", "4", "5", "5", "5", "5", "6"},
		},
		Timeout: durationpb.New(5 * time.Minute),
	})
	require.NoError(t, err)

	// Wait for the request to process.
	time.Sleep(3 * time.Second)

	t.Log("Submitting events - expecting notifications")
	expected := []*committerpb.TxStatus{
		{Ref: committerpb.NewTxRef("1", 1, 1)},
		{Ref: committerpb.NewTxRef("2", 5, 1)},
	}
	env.statusQueue.Write(expected)

	res, err := stream.Recv()
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Empty(t, res.TimeoutTxIds)
	test.RequireProtoElementsMatch(t, expected, res.TxStatusEvents)

	t.Log("Submitting more events - expecting notifications")
	expected = []*committerpb.TxStatus{
		{Ref: committerpb.NewTxRef("3", 2, 5)},
		{Ref: committerpb.NewTxRef("4", 3, 10)},
	}
	env.statusQueue.Write(expected)

	res, err = stream.Recv()
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Empty(t, res.TimeoutTxIds)
	test.RequireProtoElementsMatch(t, expected, res.TxStatusEvents)

	t.Log("Submitting requests with short timeout - expecting notifications")
	timeoutIDs := []string{"5", "6", "7", "8"}
	err = stream.Send(&committerpb.NotificationRequest{
		TxStatusRequest: &committerpb.TxIDsBatch{
			TxIds: timeoutIDs,
		},
		Timeout: durationpb.New(1 * time.Millisecond),
	})
	require.NoError(t, err)

	// Wait for the request to process.
	time.Sleep(3 * time.Second)

	res, err = stream.Recv()
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Empty(t, res.TxStatusEvents)
	require.ElementsMatch(t, timeoutIDs, res.TimeoutTxIds)

	t.Log("Submitting event with duplicate request - expecting single notification")
	expected = []*committerpb.TxStatus{
		{Ref: committerpb.NewTxRef("5", 3, 0)},
	}
	env.statusQueue.Write(expected)

	res, err = stream.Recv()
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Empty(t, res.TimeoutTxIds)
	test.RequireProtoElementsMatch(t, expected, res.TxStatusEvents)

	t.Log("Submitting duplicated event - expecting single notification")
	expected = []*committerpb.TxStatus{
		{Ref: committerpb.NewTxRef("6", 4, 0)},
		{Ref: committerpb.NewTxRef("6", 4, 1)},
		{Ref: committerpb.NewTxRef("6", 4, 2)},
	}
	env.statusQueue.Write(expected)

	res, err = stream.Recv()
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Empty(t, res.TimeoutTxIds)
	test.RequireProtoElementsMatch(t, expected[:1], res.TxStatusEvents)
}

func TestNotifierGlobalLimit(t *testing.T) {
	t.Parallel()

	env := newNotifierTestEnvWithConfig(t, 1, &NotificationServiceConfig{
		MaxTimeout:         DefaultNotificationMaxTimeout,
		MaxActiveTxIDs:     5,
		MaxTxIDsPerRequest: 10,
	})
	q := env.responseQueues[0]

	submitRequest := func(timeout time.Duration, txIDs ...string) {
		env.requestQueue.Write(&notificationRequest{
			request: &committerpb.NotificationRequest{
				TxStatusRequest: &committerpb.TxIDsBatch{TxIds: txIDs},
				Timeout:         durationpb.New(timeout),
			},
			streamEventQueue: q,
		})
	}

	requireRejected := func(expectedTxIDs []string) {
		t.Helper()
		res, ok := q.ReadWithTimeout(10 * time.Second)
		require.True(t, ok)
		require.NotNil(t, res.RejectedTxIds)
		require.ElementsMatch(t, expectedTxIDs, res.RejectedTxIds.TxIds)
		require.Contains(t, res.RejectedTxIds.Reason, "active tx IDs limit reached")
	}

	// First request: 3 txIDs, fits within limit.
	submitRequest(5*time.Minute, "1", "2", "3")

	// Second request: 4 unique txIDs (with duplicates), only 2 slots remain.
	// Duplicates are skipped before checking slots, so "4" and "5" are accepted, "6" and "7" rejected.
	// FIFO processing of the request queue guarantees the first request is handled before this one.
	submitRequest(5*time.Minute, "4", "4", "5", "5", "6", "7")
	requireRejected([]string{"6", "7"})

	// Third request: all slots still full, fully rejected.
	submitRequest(5*time.Minute, "6", "7")
	requireRejected([]string{"6", "7"})

	// Fulfil the 2 accepted txIDs from the second request.
	env.statusQueue.Write([]*committerpb.TxStatus{
		{Ref: committerpb.NewTxRef("4", 1, 0)},
		{Ref: committerpb.NewTxRef("5", 1, 1)},
	})

	res, ok := q.ReadWithTimeout(10 * time.Second)
	require.True(t, ok)
	require.Nil(t, res.RejectedTxIds)
	require.Len(t, res.TxStatusEvents, 2)

	// Timeout frees slots: fill remaining slots with a short timeout request.
	submitRequest(1*time.Millisecond, "t1", "t2")

	// Wait for timeout notification — this confirms the slots have been freed.
	res, ok = q.ReadWithTimeout(10 * time.Second)
	require.True(t, ok)
	require.ElementsMatch(t, []string{"t1", "t2"}, res.TimeoutTxIds)

	// Now 2 slots freed by timeout + 2 freed by status = 4 free (1 still held by "1","2","3" minus fulfilled).
	// "1","2","3" are still active from the first request. So 2 slots are available.
	// New request with 2 txIDs should be fully accepted (no rejection).
	submitRequest(5*time.Minute, "6", "7")

	// Fulfil to confirm they were accepted.
	env.statusQueue.Write([]*committerpb.TxStatus{
		{Ref: committerpb.NewTxRef("6", 2, 0)},
		{Ref: committerpb.NewTxRef("7", 2, 1)},
	})

	res, ok = q.ReadWithTimeout(10 * time.Second)
	require.True(t, ok)
	require.Nil(t, res.RejectedTxIds)
	require.Len(t, res.TxStatusEvents, 2)
}

func newNotifierTestEnv(tb testing.TB, numOfClients int) *notifierTestEnv {
	tb.Helper()
	return newNotifierTestEnvWithConfig(tb, numOfClients, &NotificationServiceConfig{
		MaxTimeout:         DefaultNotificationMaxTimeout,
		MaxActiveTxIDs:     DefaultMaxActiveTxIDs,
		MaxTxIDsPerRequest: DefaultMaxTxIDsPerRequest,
	})
}

func newNotifierTestEnvWithConfig(tb testing.TB, numOfClients int, conf *NotificationServiceConfig) *notifierTestEnv {
	tb.Helper()
	metrics := newPerformanceMetrics()
	env := &notifierTestEnv{
		n:              newNotifier(DefaultBufferSize, conf, metrics),
		metrics:        metrics,
		responseQueues: make([]channel.ReaderWriter[*committerpb.NotificationResponse], numOfClients),
	}
	statusQueue := make(chan []*committerpb.TxStatus, DefaultBufferSize)
	env.requestQueue = channel.NewWriter(tb.Context(), env.n.requestQueue)
	env.statusQueue = channel.NewWriter(tb.Context(), statusQueue)
	for i := range env.responseQueues {
		env.responseQueues[i] = channel.Make[*committerpb.NotificationResponse](tb.Context(), 10)
	}

	test.RunServiceForTest(tb.Context(), tb, func(ctx context.Context) error {
		return connection.FilterStreamRPCError(env.n.run(ctx, statusQueue))
	}, nil)
	return env
}
