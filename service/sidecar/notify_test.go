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

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
	"github.com/hyperledger/fabric-x-committer/api/protonotify"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/logging"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

func BenchmarkNotifier(b *testing.B) {
	logging.SetupWithConfig(&logging.Config{Enabled: false})
	txIDs := make([]string, b.N)
	for i := range txIDs {
		txIDs[i] = fmt.Sprintf("%064d", i)
	}

	batchSize := 4096
	requests := make([]*protonotify.NotificationRequest, 0, (b.N/batchSize)+1)
	statuses := make([][]*protonotify.TxStatusEvent, 0, (b.N/batchSize)+1)
	requestTxIDs := txIDs
	for len(requestTxIDs) > 0 {
		sz := min(batchSize, len(requestTxIDs))
		requests = append(requests, &protonotify.NotificationRequest{
			TxStatusRequest: &protonotify.TxStatusRequest{
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
		status := make([]*protonotify.TxStatusEvent, sz)
		for i, txID := range statusTxIDs[:sz] {
			status[i] = &protonotify.TxStatusEvent{TxId: txID}
		}
		statuses = append(statuses, status)
		statusTxIDs = statusTxIDs[sz:]
	}

	env := newNotifierTestEnv(b)
	q := env.notificationQueues[0]

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
			b.Fatalf("expected notification")
		}
		notifiedCount += len(res.TxStatusEvents)
	}
	b.StopTimer()
}

type notifierTestEnv struct {
	n                  *notifier
	requestQueue       channel.Writer[*notificationRequest]
	statusQueue        channel.Writer[[]*protonotify.TxStatusEvent]
	notificationQueues []channel.ReaderWriter[*protonotify.NotificationResponse]
}

func TestNotifierDirect(t *testing.T) {
	t.Parallel()
	env := newNotifierTestEnv(t)

	t.Log("Submitting requests")
	for _, q := range env.notificationQueues {
		env.requestQueue.Write(&notificationRequest{
			request: &protonotify.NotificationRequest{
				TxStatusRequest: &protonotify.TxStatusRequest{
					TxIds: []string{"1", "2", "3", "4", "5", "5", "5", "5", "6"},
				},
				Timeout: durationpb.New(5 * time.Minute),
			},
			streamEventQueue: q,
		})
	}

	t.Log("No events - not expecting notifications")
	time.Sleep(3 * time.Second)
	for _, q := range env.notificationQueues {
		_, ok := q.ReadWithTimeout(10 * time.Millisecond)
		require.False(t, ok, "should not receive notification")
	}

	t.Log("Submitting events - expecting notifications")
	expected := []*protonotify.TxStatusEvent{
		{
			TxId: "1",
			StatusWithHeight: &protoblocktx.StatusWithHeight{
				BlockNumber: 1,
				TxNumber:    1,
			},
		},
		{
			TxId: "2",
			StatusWithHeight: &protoblocktx.StatusWithHeight{
				BlockNumber: 5,
				TxNumber:    1,
			},
		},
	}
	env.statusQueue.Write(expected)
	for _, q := range env.notificationQueues {
		res, ok := q.ReadWithTimeout(10 * time.Second)
		require.True(t, ok)
		require.NotNil(t, res)
		require.Empty(t, res.TimeoutTxIds)
		test.RequireProtoElementsMatch(t, expected, res.TxStatusEvents)
	}

	t.Log("Not expecting more notifications")
	time.Sleep(3 * time.Second)
	for _, q := range env.notificationQueues {
		_, ok := q.ReadWithTimeout(10 * time.Millisecond)
		require.False(t, ok, "should not receive notification")
	}

	t.Log("Submitting irrelevant events - not expecting notifications")
	env.statusQueue.Write([]*protonotify.TxStatusEvent{
		{
			TxId: "100",
			StatusWithHeight: &protoblocktx.StatusWithHeight{
				BlockNumber: 1,
				TxNumber:    1,
			},
		},
		{
			TxId: "200",
			StatusWithHeight: &protoblocktx.StatusWithHeight{
				BlockNumber: 5,
				TxNumber:    1,
			},
		},
	})
	time.Sleep(3 * time.Second)
	for _, q := range env.notificationQueues {
		_, ok := q.ReadWithTimeout(10 * time.Millisecond)
		require.False(t, ok, "should not receive notification")
	}

	t.Log("Submitting more events - expecting notifications")
	expected = []*protonotify.TxStatusEvent{
		{
			TxId: "3",
			StatusWithHeight: &protoblocktx.StatusWithHeight{
				BlockNumber: 2,
				TxNumber:    5,
			},
		},
		{
			TxId: "4",
			StatusWithHeight: &protoblocktx.StatusWithHeight{
				BlockNumber: 3,
				TxNumber:    10,
			},
		},
	}
	env.statusQueue.Write(expected)
	for _, q := range env.notificationQueues {
		res, ok := q.Read()
		require.True(t, ok)
		require.NotNil(t, res)
		require.Empty(t, res.TimeoutTxIds)
		test.RequireProtoElementsMatch(t, expected, res.TxStatusEvents)
	}

	t.Log("Submitting requests with short timeout - expecting notifications")
	timeoutIDs := []string{"5", "6", "7", "8"}
	for _, q := range env.notificationQueues {
		env.requestQueue.Write(&notificationRequest{
			request: &protonotify.NotificationRequest{
				TxStatusRequest: &protonotify.TxStatusRequest{
					TxIds: timeoutIDs,
				},
				Timeout: durationpb.New(1 * time.Millisecond),
			},
			streamEventQueue: q,
		})
	}
	for _, q := range env.notificationQueues {
		res, ok := q.ReadWithTimeout(10 * time.Second)
		require.True(t, ok)
		require.NotNil(t, res)
		require.Empty(t, res.TxStatusEvents)
		require.ElementsMatch(t, timeoutIDs, res.TimeoutTxIds)
	}

	t.Log("Submitting event with duplicate request - expecting single notification")
	expected = []*protonotify.TxStatusEvent{
		{
			TxId: "5",
			StatusWithHeight: &protoblocktx.StatusWithHeight{
				BlockNumber: 3,
				TxNumber:    0,
			},
		},
	}
	env.statusQueue.Write(expected)
	for _, q := range env.notificationQueues {
		res, ok := q.Read()
		require.True(t, ok)
		require.NotNil(t, res)
		require.Empty(t, res.TimeoutTxIds)
		test.RequireProtoElementsMatch(t, expected, res.TxStatusEvents)
	}

	t.Log("Submitting duplicated event - expecting single notification")
	expected = []*protonotify.TxStatusEvent{
		{
			TxId: "6",
			StatusWithHeight: &protoblocktx.StatusWithHeight{
				BlockNumber: 4,
				TxNumber:    0,
			},
		},
		{
			TxId: "6",
			StatusWithHeight: &protoblocktx.StatusWithHeight{
				BlockNumber: 4,
				TxNumber:    1,
			},
		},
		{
			TxId: "6",
			StatusWithHeight: &protoblocktx.StatusWithHeight{
				BlockNumber: 4,
				TxNumber:    2,
			},
		},
	}
	env.statusQueue.Write(expected)
	for _, q := range env.notificationQueues {
		res, ok := q.Read()
		require.True(t, ok)
		require.NotNil(t, res)
		require.Empty(t, res.TimeoutTxIds)
		test.RequireProtoElementsMatch(t, expected[:1], res.TxStatusEvents)
	}
}

func TestNotifierStream(t *testing.T) {
	t.Parallel()
	env := newNotifierTestEnv(t)
	config := connection.NewLocalHostServerWithTLS(test.DefaultTLSConfig)
	test.RunGrpcServerForTest(t.Context(), t, config, func(server *grpc.Server) {
		protonotify.RegisterNotifierServer(server, env.n)
	})
	endpoint := &config.Endpoint
	conn, err := connection.Connect(test.NewInsecureDialConfig(endpoint))
	require.NoError(t, err)
	client := protonotify.NewNotifierClient(conn)

	stream, err := client.OpenNotificationStream(t.Context())
	require.NoError(t, err)

	t.Log("Submitting requests")
	err = stream.Send(&protonotify.NotificationRequest{
		TxStatusRequest: &protonotify.TxStatusRequest{
			TxIds: []string{"1", "2", "3", "4", "5", "5", "5", "5", "6"},
		},
		Timeout: durationpb.New(5 * time.Minute),
	})
	require.NoError(t, err)

	// Wait for the request to process.
	time.Sleep(3 * time.Second)

	t.Log("Submitting events - expecting notifications")
	expected := []*protonotify.TxStatusEvent{
		{
			TxId: "1",
			StatusWithHeight: &protoblocktx.StatusWithHeight{
				BlockNumber: 1,
				TxNumber:    1,
			},
		},
		{
			TxId: "2",
			StatusWithHeight: &protoblocktx.StatusWithHeight{
				BlockNumber: 5,
				TxNumber:    1,
			},
		},
	}
	env.statusQueue.Write(expected)

	res, err := stream.Recv()
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Empty(t, res.TimeoutTxIds)
	test.RequireProtoElementsMatch(t, expected, res.TxStatusEvents)

	t.Log("Submitting more events - expecting notifications")
	expected = []*protonotify.TxStatusEvent{
		{
			TxId: "3",
			StatusWithHeight: &protoblocktx.StatusWithHeight{
				BlockNumber: 2,
				TxNumber:    5,
			},
		},
		{
			TxId: "4",
			StatusWithHeight: &protoblocktx.StatusWithHeight{
				BlockNumber: 3,
				TxNumber:    10,
			},
		},
	}
	env.statusQueue.Write(expected)

	res, err = stream.Recv()
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Empty(t, res.TimeoutTxIds)
	test.RequireProtoElementsMatch(t, expected, res.TxStatusEvents)

	t.Log("Submitting requests with short timeout - expecting notifications")
	timeoutIDs := []string{"5", "6", "7", "8"}
	err = stream.Send(&protonotify.NotificationRequest{
		TxStatusRequest: &protonotify.TxStatusRequest{
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
	expected = []*protonotify.TxStatusEvent{
		{
			TxId: "5",
			StatusWithHeight: &protoblocktx.StatusWithHeight{
				BlockNumber: 3,
				TxNumber:    0,
			},
		},
	}
	env.statusQueue.Write(expected)

	res, err = stream.Recv()
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Empty(t, res.TimeoutTxIds)
	test.RequireProtoElementsMatch(t, expected, res.TxStatusEvents)

	t.Log("Submitting duplicated event - expecting single notification")
	expected = []*protonotify.TxStatusEvent{
		{
			TxId: "6",
			StatusWithHeight: &protoblocktx.StatusWithHeight{
				BlockNumber: 4,
				TxNumber:    0,
			},
		},
		{
			TxId: "6",
			StatusWithHeight: &protoblocktx.StatusWithHeight{
				BlockNumber: 4,
				TxNumber:    1,
			},
		},
		{
			TxId: "6",
			StatusWithHeight: &protoblocktx.StatusWithHeight{
				BlockNumber: 4,
				TxNumber:    2,
			},
		},
	}
	env.statusQueue.Write(expected)

	res, err = stream.Recv()
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Empty(t, res.TimeoutTxIds)
	test.RequireProtoElementsMatch(t, expected[:1], res.TxStatusEvents)
}

func newNotifierTestEnv(tb testing.TB) *notifierTestEnv {
	tb.Helper()
	env := &notifierTestEnv{
		n:                  newNotifier(defaultBufferSize, &NotificationServiceConfig{}),
		notificationQueues: make([]channel.ReaderWriter[*protonotify.NotificationResponse], 5),
	}
	statusQueue := make(chan []*protonotify.TxStatusEvent, defaultBufferSize)
	env.requestQueue = channel.NewWriter(tb.Context(), env.n.requestQueue)
	env.statusQueue = channel.NewWriter(tb.Context(), statusQueue)
	for i := range env.notificationQueues {
		env.notificationQueues[i] = channel.Make[*protonotify.NotificationResponse](tb.Context(), 10)
	}

	test.RunServiceForTest(tb.Context(), tb, func(ctx context.Context) error {
		return connection.FilterStreamRPCError(env.n.run(ctx, statusQueue))
	}, nil)
	return env
}
