/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package adapters

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	commontypes "github.com/hyperledger/fabric-x-common/api/types"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/mock"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/ordererconn"
	"github.com/hyperledger/fabric-x-committer/utils/serialization"
	"github.com/hyperledger/fabric-x-committer/utils/test"
	"github.com/hyperledger/fabric-x-committer/utils/testcrypto"
)

const channelForTest = "my-channel"

type (
	broadcastTestEnv struct {
		*mock.OrdererTestEnv
		fakeServers map[uint32][]*commontypes.OrdererEndpoint
		downServers map[uint32][]*commontypes.OrdererEndpoint
		nextBlock   uint64
	}

	expectedSubmit struct {
		success       int
		unavailable   int
		unimplemented int
	}
)

func TestBroadcast(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		tlsMode string
		ftLevel string
	}{
		{tlsMode: connection.NoneTLSMode, ftLevel: ordererconn.BFT},
		{tlsMode: connection.OneSideTLSMode, ftLevel: ordererconn.BFT},
		{tlsMode: connection.MutualTLSMode, ftLevel: ordererconn.BFT},
		{tlsMode: connection.MutualTLSMode, ftLevel: ordererconn.CFT},
	} {
		t.Run(fmt.Sprintf("tls-mode:%s %s", tc.tlsMode, tc.ftLevel), func(t *testing.T) {
			t.Parallel()
			e := newBroadcastTestEnv(t, tc.tlsMode, tc.ftLevel)
			expectedSuccess := 1
			if tc.ftLevel == ordererconn.BFT {
				expectedSuccess = 3
			}
			e.submit(t, e.AllEndpoints, expectedSubmit{success: expectedSuccess})
		})
	}
}

func TestBroadcastBFT(t *testing.T) {
	t.Parallel()

	e := newBroadcastTestEnv(t, connection.MutualTLSMode, ordererconn.BFT)
	t.Log("All good")
	e.submit(t, e.AllEndpoints, expectedSubmit{
		success: 3,
	})

	t.Log("One server down")
	ep0 := e.EndpointsOfID(0)
	ep1 := e.EndpointsOfID(1)
	ep2 := e.EndpointsOfID(2)
	ep := slices.Concat(ep0, ep1, ep2[:1], e.downServers[2][:1])
	e.submit(t, ep, expectedSubmit{
		success: 3,
	})

	t.Log("Two servers down")
	ep = slices.Concat(ep0, ep1, e.downServers[2])
	e.submit(t, ep, expectedSubmit{
		success:     2,
		unavailable: 1,
	})

	t.Log("One incorrect server")
	ep = slices.Concat(ep0, ep1, e.fakeServers[2][:1])
	e.submit(t, ep, expectedSubmit{
		success:       2,
		unimplemented: 1,
	})

	t.Log("Insufficient quorum")
	ep = slices.Concat(ep0, e.downServers[1], e.downServers[2])
	e.submit(t, ep, expectedSubmit{
		success:     1,
		unavailable: 2,
	})
}

func newBroadcastTestEnv(t *testing.T, tlsMode, ftLevel string) *broadcastTestEnv {
	t.Helper()
	// Create the credentials for both server and client using the same CA.
	// In this test, we are using the same server TLS config for all the orderer instances for simplicity.
	serverTLSConfig, clientTLSConfig := test.CreateServerAndClientTLSConfig(t, tlsMode)
	e := mock.NewOrdererTestEnv(t, &mock.OrdererTestParameters{
		ChanID:          channelForTest,
		NumIDs:          3,
		ServerPerID:     2,
		ServerTLSConfig: serverTLSConfig,
		ClientTLSConfig: clientTLSConfig,
		OrdererConfig: &mock.OrdererConfig{
			BlockSize:        1,
			SendGenesisBlock: true,
		},
	})
	e.OrdererConnConfig.FaultToleranceLevel = ftLevel

	sc := make([]*connection.ServerConfig, 0, len(e.AllServerConfig))
	fakeEndpoints := make(map[uint32][]*commontypes.OrdererEndpoint)
	downServers := make(map[uint32][]*commontypes.OrdererEndpoint)
	for id := range e.NumIDs {
		fakeEp := make([]*commontypes.OrdererEndpoint, e.ServerPerID)
		downEp := make([]*commontypes.OrdererEndpoint, e.ServerPerID)
		for i := range e.ServerPerID {
			fakeServer := test.NewPreAllocatedLocalHostServer(t, serverTLSConfig)
			sc = append(sc, fakeServer)
			fakeEp[i] = &commontypes.OrdererEndpoint{
				ID:   id,
				Host: sc[i].Endpoint.Host,
				Port: sc[i].Endpoint.Port,
			}

			downServer := test.NewPreAllocatedLocalHostServer(t, serverTLSConfig)
			downEp[i] = &commontypes.OrdererEndpoint{
				ID:   id,
				Host: downServer.Endpoint.Host,
				Port: downServer.Endpoint.Port,
			}
		}
		fakeEndpoints[id] = fakeEp
		downServers[id] = downEp
	}
	fakeServers := test.StartGrpcServersWithConfigForTest(t.Context(), t, nil, sc...)
	require.Len(t, fakeServers.Servers, len(e.AllServerConfig))

	t.Log("Read config block")
	b := e.GetBlock(t, 0)
	require.Len(t, b.Data.Data, 1)
	return &broadcastTestEnv{
		OrdererTestEnv: e,
		fakeServers:    fakeEndpoints,
		downServers:    downServers,
		nextBlock:      1,
	}
}

func (e *broadcastTestEnv) submit(
	t *testing.T,
	endpoints []*commontypes.OrdererEndpoint,
	expected expectedSubmit,
) {
	t.Helper()
	e.SubmitConfigBlock(t, &testcrypto.ConfigBlock{OrdererEndpoints: endpoints})
	b := e.GetBlock(t, e.nextBlock)
	require.Len(t, b.Data.Data, 1)
	e.nextBlock++

	// We create a new stream for each request to ensure GRPC does not cache the latest state.
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	stream, err := NewBroadcastStream(ctx, &e.OrdererConnConfig)
	require.NoError(t, err)
	defer stream.CloseConnections()

	txb := workload.TxBuilder{ChannelID: channelForTest}
	txs := []*servicepb.LoadGenTx{txb.MakeTx(&applicationpb.Tx{})}
	err = stream.SendBatch(workload.MapToEnvelopeBatch(0, txs))
	if err != nil {
		t.Logf("Response error:\n%s", err)
	}
	if expected.unavailable > 0 {
		require.Error(t, err)
	} else if expected.unimplemented == 0 {
		// Unimplemented sometimes not responding with error, so we cannot enforce it.
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

	b = e.GetBlock(t, e.nextBlock)
	e.nextBlock++
	require.Len(t, b.Data.Data, len(txs))
	for i, d := range b.Data.Data {
		_, hdr, marshalErr := serialization.UnwrapEnvelope(d)
		require.NoError(t, marshalErr)
		require.Equal(t, txs[i].Id, hdr.TxId)
	}
}
