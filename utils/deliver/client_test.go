/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package deliver

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
	"github.com/hyperledger/fabric-x-committer/api/protoloadgen"
	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/mock"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/ordererconn"
	"github.com/hyperledger/fabric-x-committer/utils/serialization"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

var testGrpcRetryProfile = connection.RetryProfile{
	InitialInterval: 10 * time.Millisecond,
	MaxInterval:     100 * time.Millisecond,
	Multiplier:      2,
	MaxElapsedTime:  time.Second,
}

const channelForTest = "mychannel"

func TestBroadcastDeliver(t *testing.T) {
	t.Parallel()
	// We use a short retry grpc-config to shorten the test time.
	ordererService, servers, conf := makeConfig(t)

	allEndpoints := conf.Connection.Endpoints

	// We only take the bottom endpoints for now.
	// Later we take the other endpoints and update the client.
	conf.Connection.Endpoints = allEndpoints[:6]
	client, err := New(&conf)
	require.NoError(t, err)
	t.Cleanup(client.Close)

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(cancel)
	stream, err := test.NewBroadcastStream(ctx, &conf)
	require.NoError(t, err)
	t.Cleanup(stream.Close)
	outputBlocksChan := make(chan *common.Block, 100)
	go func() {
		p := &Parameters{
			StartBlkNum: 0,
			EndBlkNum:   MaxBlockNum,
			OutputBlock: outputBlocksChan,
		}
		// We set the client to stop retry after a second to quickly
		// receive the broadcast errors.
		// But for delivery, we want to continue indefinitely.
		for ctx.Err() == nil {
			err = client.Deliver(ctx, p)
			t.Logf("Deliver ended with: %v", err)
		}
	}()
	outputBlocks := channel.NewReader(ctx, outputBlocksChan)

	t.Log("Read config block")
	b, ok := outputBlocks.Read()
	require.True(t, ok)
	require.NotNil(t, b)
	require.Len(t, b.Data.Data, 1)

	t.Log("All good")
	submit(t, stream, outputBlocks, expectedSubmit{
		success: 3,
	})

	t.Log("One server down")
	servers[2].Servers[0].Stop()
	time.Sleep(3 * time.Second)
	submit(t, stream, outputBlocks, expectedSubmit{
		success: 3,
	})

	t.Log("Two servers down")
	servers[2].Servers[1].Stop()
	time.Sleep(3 * time.Second)
	submit(t, stream, outputBlocks, expectedSubmit{
		success:     2,
		unavailable: 1,
	})

	t.Log("One incorrect server")
	fakeServer := test.RunGrpcServerForTest(ctx, t, servers[2].Configs[0], nil)
	waitUntilGrpcServerIsReady(ctx, t, &servers[2].Configs[0].Endpoint)

	submit(t, stream, outputBlocks, expectedSubmit{
		success:       2,
		unimplemented: 1,
	})

	t.Log("All good again")
	fakeServer.Stop()
	servers[2].Servers[0] = test.RunGrpcServerForTest(ctx, t, servers[2].Configs[0], ordererService.RegisterService)
	waitUntilGrpcServerIsReady(ctx, t, &servers[2].Configs[0].Endpoint)
	submit(t, stream, outputBlocks, expectedSubmit{
		success: 3,
	})

	t.Log("Insufficient quorum")
	servers[0].Servers[0].Stop()
	servers[0].Servers[1].Stop()
	servers[1].Servers[0].Stop()
	servers[1].Servers[1].Stop()
	time.Sleep(3 * time.Second)
	submit(t, stream, outputBlocks, expectedSubmit{
		success:     1,
		unavailable: 2,
	})

	t.Log("Update endpoints")
	conf.Connection.Endpoints = allEndpoints[6:]
	err = client.UpdateConnections(&conf.Connection)
	require.NoError(t, err)
	require.NoError(t, stream.ConnectionManager.Update(&conf.Connection))
	submit(t, stream, outputBlocks, expectedSubmit{
		success: 3,
	})
}

type expectedSubmit struct {
	success       int
	unavailable   int
	unimplemented int
}

func submit(
	t *testing.T,
	stream *test.BroadcastStream,
	outputBlocks channel.Reader[*common.Block],
	expected expectedSubmit,
) {
	t.Helper()
	txb := workload.TxBuilder{ChannelID: channelForTest}
	tx := txb.MakeTx(&protoblocktx.Tx{})

	err := stream.SendBatch(workload.MapToEnvelopeBatch(0, []*protoloadgen.TX{tx}))
	if err != nil {
		t.Logf("Response error:\n%s", err)
	}
	if expected.unavailable > 0 {
		// Unimplemented sometimes not responding with error, so we cannot enforce it.
		require.Error(t, err)
	} else if expected.unimplemented == 0 {
		require.NoError(t, err)
	}

	if err != nil {
		errStr := err.Error()
		// We verify that we didn't get more Unavailable or Unimplemented than expected.
		// Unimplemented sometimes returns Unavailable or no error at all, so we might not get exact value.
		unavailableCount := strings.Count(errStr, "Unavailable")
		unimplementedCount := strings.Count(errStr, "Unimplemented")
		require.LessOrEqual(t, unavailableCount, expected.unavailable+expected.unimplemented, err)
		require.LessOrEqual(t, unimplementedCount, expected.unimplemented, err)
	}

	b, ok := outputBlocks.Read()
	require.True(t, ok)
	require.NotNil(t, b)
	require.Len(t, b.Data.Data, 1)
	_, hdr, err := serialization.UnwrapEnvelope(b.Data.Data[0])
	require.NoError(t, err)
	require.Equal(t, tx.Id, hdr.TxId)
}

func makeConfig(t *testing.T) (*mock.Orderer, []test.GrpcServers, ordererconn.Config) {
	t.Helper()

	idCount := 3
	serverPerID := 4
	instanceCount := idCount * serverPerID
	t.Logf("Instance count: %d; idCount: %d", instanceCount, idCount)
	ordererService, ordererServer := mock.StartMockOrderingServices(
		t, &mock.OrdererConfig{NumService: instanceCount, BlockSize: 1, SendConfigBlock: true},
	)
	require.Len(t, ordererServer.Servers, instanceCount)

	conf := ordererconn.Config{
		ChannelID:     channelForTest,
		ConsensusType: ordererconn.Bft,
		Connection:    ordererconn.ConnectionConfig{Retry: &testGrpcRetryProfile},
	}
	servers := make([]test.GrpcServers, idCount)
	for i, c := range ordererServer.Configs {
		id := uint32(i % idCount) //nolint:gosec // integer overflow conversion int -> uint32
		conf.Connection.Endpoints = append(conf.Connection.Endpoints, &ordererconn.Endpoint{
			ID:       id,
			Endpoint: c.Endpoint,
		})
		s := &servers[id]
		s.Configs = append(s.Configs, c)
		s.Servers = append(s.Servers, ordererServer.Servers[i])
	}
	for i, s := range servers {
		require.Lenf(t, s.Configs, serverPerID, "id: %d", i)
		require.Lenf(t, s.Servers, serverPerID, "id: %d", i)
	}
	for i, e := range conf.Connection.Endpoints {
		t.Logf("ENDPOINT [%02d] %s", i, e.String())
	}
	return ordererService, servers, conf
}

func waitUntilGrpcServerIsReady(ctx context.Context, t *testing.T, endpoint *connection.Endpoint) {
	t.Helper()
	newConn, err := connection.Connect(test.NewInsecureDialConfig(endpoint))
	require.NoError(t, err)
	defer connection.CloseConnectionsLog(newConn)
	test.WaitUntilGrpcServerIsReady(ctx, t, newConn)
	t.Logf("%v is ready", endpoint)
	// Wait a while to allow the GRPC connection enough time to make a new attempt.
	time.Sleep(time.Second)
}
