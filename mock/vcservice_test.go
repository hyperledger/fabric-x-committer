/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package mock

import (
	"testing"

	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	"github.com/hyperledger/fabric-x-committer/utils"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

type mockVCTestEnv struct {
	vc      *VcService
	servers *test.Servers
	conns   []*grpc.ClientConn
	clients []servicepb.ValidationAndCommitServiceClient
	streams []grpc.BidiStreamingClient[servicepb.VcBatch, committerpb.TxStatusBatch]
}

const testNS = "ns1"

func startVCTestEnv(t *testing.T, p test.StartServerParameters) *mockVCTestEnv {
	t.Helper()
	vc, serverConfig := StartMockVCService(t, p)
	require.NotNil(t, vc)

	// Verify no streams initially
	RequireStreams(t, vc, 0)

	conns := make([]*grpc.ClientConn, len(serverConfig.Configs))
	clients := make([]servicepb.ValidationAndCommitServiceClient, len(serverConfig.Configs))
	streams := make([]grpc.BidiStreamingClient[servicepb.VcBatch, committerpb.TxStatusBatch], len(serverConfig.Configs))
	for i, cfg := range serverConfig.Configs {
		conn := test.NewInsecureConnection(t, &cfg.GRPC.Endpoint)
		client := servicepb.NewValidationAndCommitServiceClient(conn)
		stream, err := client.StartValidateAndCommitStream(t.Context())
		require.NoError(t, err)

		conns[i] = conn
		clients[i] = client
		streams[i] = stream
	}

	// Verify streams are registered
	RequireStreams(t, vc, len(streams))

	return &mockVCTestEnv{
		vc:      vc,
		servers: serverConfig,
		conns:   conns,
		clients: clients,
		streams: streams,
	}
}

func (e *mockVCTestEnv) sendTX(t *testing.T, tx *servicepb.VcTx, expectedStatus committerpb.Status) {
	t.Helper()
	e.sendBatch(t, &servicepb.VcBatch{Transactions: []*servicepb.VcTx{tx}}, []committerpb.Status{expectedStatus})
}

func (e *mockVCTestEnv) sendBatch(t *testing.T, batch *servicepb.VcBatch, expectedStatus []committerpb.Status) {
	t.Helper()
	require.NotNil(t, batch)
	err := e.streams[0].Send(batch)
	require.NoError(t, err)

	statusBatch, err := e.streams[0].Recv()
	require.NoError(t, err)
	require.Len(t, statusBatch.Status, len(batch.Transactions))
	require.Len(t, expectedStatus, len(batch.Transactions))
	for i, s := range statusBatch.Status {
		txRef := batch.Transactions[i].Ref
		require.EqualExportedValues(t, txRef, s.Ref)
		expected := expectedStatus[i] //nolint:gosec // false positive - slice index out of range.
		require.Equalf(t, expected.String(), s.Status.String(), "tx-ref: %v", &utils.LazyJSON{O: txRef})
	}
}

// TestVcService tests the mock VC service implementation through its gRPC interface.
func TestVcService(t *testing.T) {
	t.Parallel()

	e := startVCTestEnv(t, test.StartServerParameters{NumService: 1})

	t.Run("set and get block number", func(t *testing.T) {
		t.Parallel()

		// Set last committed block number
		_, err := e.clients[0].SetLastCommittedBlockNumber(t.Context(), &servicepb.BlockRef{Number: 10})
		require.NoError(t, err)

		// Get next block number (should be 11)
		nextBlock, err := e.clients[0].GetNextBlockNumberToCommit(t.Context(), nil)
		require.NoError(t, err)
		require.NotNil(t, nextBlock)
		require.EqualValues(t, 11, nextBlock.Number)
	})

	t.Run("get namespace policies", func(t *testing.T) {
		t.Parallel()
		policies, err := e.clients[0].GetNamespacePolicies(t.Context(), nil)
		require.NoError(t, err)
		require.NotNil(t, policies)
	})

	t.Run("get config transaction", func(t *testing.T) {
		t.Parallel()
		configTx, err := e.clients[0].GetConfigTransaction(t.Context(), nil)
		require.NoError(t, err)
		require.NotNil(t, configTx)
	})

	t.Run("setup system tables", func(t *testing.T) {
		t.Parallel()
		_, err := e.clients[0].SetupSystemTablesAndNamespaces(t.Context(), nil)
		require.NoError(t, err)
	})
}

// TestVcServiceStreamProcessing tests the transaction processing pipeline through streaming.
func TestVcServiceStreamProcessing(t *testing.T) {
	t.Parallel()

	t.Run("process valid transactions", func(t *testing.T) {
		t.Parallel()

		e := startVCTestEnv(t, test.StartServerParameters{NumService: 1})

		// Send batch of transactions
		batch := &servicepb.VcBatch{
			Transactions: []*servicepb.VcTx{
				{
					Ref: committerpb.NewTxRef("tx1", 1, 0),
					Namespaces: []*applicationpb.TxNamespace{{
						NsId: testNS,
						BlindWrites: []*applicationpb.Write{
							{Key: []byte("key1"), Value: []byte("value1")},
						},
					}},
				},
				{
					Ref: committerpb.NewTxRef("tx2", 2, 1),
					Namespaces: []*applicationpb.TxNamespace{{
						NsId: testNS,
						BlindWrites: []*applicationpb.Write{
							{Key: []byte("key2"), Value: []byte("value2")},
						},
					}},
				},
			},
		}
		e.sendBatch(t, batch, []committerpb.Status{committerpb.Status_COMMITTED, committerpb.Status_COMMITTED})

		// Verify batch counter
		require.Equal(t, uint32(1), e.vc.NumBatchesReceived.Load())
	})

	t.Run("process transactions with preliminary invalid status", func(t *testing.T) {
		t.Parallel()

		e := startVCTestEnv(t, test.StartServerParameters{NumService: 1})
		tx := &servicepb.VcTx{
			Ref:                   committerpb.NewTxRef("tx-invalid", 10, 0),
			PrelimInvalidTxStatus: new(committerpb.Status_MALFORMED_NO_WRITES),
			Namespaces: []*applicationpb.TxNamespace{{
				NsId:      testNS,
				ReadsOnly: []*applicationpb.Read{{Key: []byte("key3")}},
			}},
		}
		e.sendTX(t, tx, committerpb.Status_MALFORMED_NO_WRITES)
	})

	t.Run("multiple batches processing", func(t *testing.T) {
		t.Parallel()

		e := startVCTestEnv(t, test.StartServerParameters{NumService: 1})

		// Send multiple batches
		for i := range 3 {
			tx := &servicepb.VcTx{
				Ref: committerpb.NewTxRef("batch-tx-"+string(rune(i)), uint64(100+i), 0),
				Namespaces: []*applicationpb.TxNamespace{{
					NsId: testNS,
					BlindWrites: []*applicationpb.Write{
						{Key: []byte("batch-key"), Value: []byte("batch-value")},
					},
				}},
			}
			e.sendTX(t, tx, committerpb.Status_COMMITTED)
		}
	})
}

// TestVcServiceStatusInjection tests that the mock returns injected statuses without
// performing any validation of its own, and defaults to COMMITTED otherwise.
func TestVcServiceStatusInjection(t *testing.T) {
	t.Parallel()

	e := startVCTestEnv(t, test.StartServerParameters{NumService: 1})

	t.Log("Case 1: No injected status defaults to COMMITTED")
	e.sendTX(t, &servicepb.VcTx{
		Ref: committerpb.NewTxRef("tx-default", 1, 0),
		Namespaces: []*applicationpb.TxNamespace{{
			NsId: testNS,
			BlindWrites: []*applicationpb.Write{
				{Key: []byte("key1"), Value: []byte("value1")},
			},
		}},
	}, committerpb.Status_COMMITTED)

	t.Log("Case 2: Injected statuses are returned verbatim")
	for i, s := range []committerpb.Status{
		committerpb.Status_ABORTED_MVCC_CONFLICT,
		committerpb.Status_REJECTED_DUPLICATE_TX_ID,
		committerpb.Status_ABORTED_SIGNATURE_INVALID,
	} {
		ref := committerpb.NewTxRef("tx-injected-"+s.String(), uint64(2+i), uint32(i))
		e.vc.SetTxStatus(ref, s)
		e.sendTX(t, &servicepb.VcTx{
			Ref: ref,
			Namespaces: []*applicationpb.TxNamespace{{
				NsId: testNS,
				BlindWrites: []*applicationpb.Write{
					{Key: []byte("inj-key"), Value: []byte("inj-value")},
				},
			}},
		}, s)
	}

	t.Log("Case 3: PrelimInvalidTxStatus takes precedence over an injected status")
	prelimRef := committerpb.NewTxRef("tx-prelim", 10, 0)
	// Even though we inject COMMITTED, the preliminary status wins.
	e.vc.SetTxStatus(prelimRef, committerpb.Status_COMMITTED)
	e.sendTX(t, &servicepb.VcTx{
		Ref:                   prelimRef,
		PrelimInvalidTxStatus: new(committerpb.Status_MALFORMED_NO_WRITES),
		Namespaces: []*applicationpb.TxNamespace{{
			NsId:      testNS,
			ReadsOnly: []*applicationpb.Read{{Key: []byte("key3")}},
		}},
	}, committerpb.Status_MALFORMED_NO_WRITES)

	t.Log("Case 4: Duplicate transaction number detection (error, not a status)")
	batchDupTxNum := &servicepb.VcBatch{
		Transactions: []*servicepb.VcTx{
			{
				Ref: committerpb.NewTxRef("tx-dup-num-1", 14, 0),
				Namespaces: []*applicationpb.TxNamespace{{
					NsId: testNS,
					BlindWrites: []*applicationpb.Write{
						{Key: []byte("key-dup-1"), Value: []byte("value1")},
					},
				}},
			},
			{
				Ref: committerpb.NewTxRef("tx-dup-num-2", 14, 0), // Duplicate tx number
				Namespaces: []*applicationpb.TxNamespace{{
					NsId: testNS,
					BlindWrites: []*applicationpb.Write{
						{Key: []byte("key-dup-2"), Value: []byte("value2")},
					},
				}},
			},
		},
	}
	err := e.streams[0].Send(batchDupTxNum)
	require.NoError(t, err)

	// Should receive error due to duplicate tx num
	_, err = e.streams[0].Recv()
	require.Error(t, err)
	require.ErrorContains(t, err, "duplication tx num detected")
}

// TestVcServiceGetTransactionsStatus tests transaction status retrieval.
func TestVcServiceGetTransactionsStatus(t *testing.T) {
	t.Parallel()

	e := startVCTestEnv(t, test.StartServerParameters{NumService: 1})

	batch := &servicepb.VcBatch{
		Transactions: []*servicepb.VcTx{
			{
				Ref: committerpb.NewTxRef("status-tx-1", 1, 0),
				Namespaces: []*applicationpb.TxNamespace{{
					NsId: testNS,
					BlindWrites: []*applicationpb.Write{
						{Key: []byte("key1"), Value: []byte("value1")},
					},
				}},
			},
			{
				Ref: committerpb.NewTxRef("status-tx-2", 2, 1),
				Namespaces: []*applicationpb.TxNamespace{{
					NsId: testNS,
					BlindWrites: []*applicationpb.Write{
						{Key: []byte("key2"), Value: []byte("value2")},
					},
				}},
			},
		},
	}
	e.sendBatch(t, batch, []committerpb.Status{committerpb.Status_COMMITTED, committerpb.Status_COMMITTED})

	// Query transaction status
	statusBatch, err := e.clients[0].GetTransactionsStatus(t.Context(), &committerpb.TxIDsBatch{
		TxIds: []string{"status-tx-1", "status-tx-2", "non-existent-tx"},
	})
	require.NoError(t, err)
	require.NotNil(t, statusBatch)

	// We only get the existing TX IDs.
	require.Len(t, statusBatch.Status, 2)
	require.NotNil(t, statusBatch.Status[0])
	require.Equal(t, "status-tx-1", statusBatch.Status[0].Ref.TxId)
	require.Equal(t, committerpb.Status_COMMITTED, statusBatch.Status[0].Status)

	require.NotNil(t, statusBatch.Status[1])
	require.Equal(t, "status-tx-2", statusBatch.Status[1].Ref.TxId)
	require.Equal(t, committerpb.Status_COMMITTED, statusBatch.Status[1].Status)
}

// TestVcServiceMultipleStreams tests multiple concurrent streams.
func TestVcServiceMultipleStreams(t *testing.T) {
	t.Parallel()

	e := startVCTestEnv(t, test.StartServerParameters{NumService: 3})

	// Send transactions on each stream (server)
	for i, stream := range e.streams {
		batch := &servicepb.VcBatch{
			Transactions: []*servicepb.VcTx{{
				Ref: committerpb.NewTxRef("multi-stream-tx-"+string(rune(i)), uint64(i+1), 0),
				Namespaces: []*applicationpb.TxNamespace{{
					NsId: testNS,
					BlindWrites: []*applicationpb.Write{
						{Key: []byte("key"), Value: []byte("value")},
					},
				}},
			}},
		}

		err := stream.Send(batch)
		require.NoError(t, err)

		statusBatch, err := stream.Recv()
		require.NoError(t, err)
		require.Len(t, statusBatch.Status, 1)
		require.Equal(t, committerpb.Status_COMMITTED, statusBatch.Status[0].Status)
	}
}

// TestVcServiceGetReceivedTxOrder verifies that GetReceivedTxOrder returns TxIds
// in the order they were processed, across batches and excluding faulty-dropped TXs.
func TestVcServiceGetReceivedTxOrder(t *testing.T) {
	t.Parallel()

	e := startVCTestEnv(t, test.StartServerParameters{NumService: 1})

	// Send two batches sequentially; order must reflect processing sequence.
	batch1 := &servicepb.VcBatch{
		Transactions: []*servicepb.VcTx{
			{Ref: committerpb.NewTxRef("order-tx-1", 1, 0)},
			{Ref: committerpb.NewTxRef("order-tx-2", 1, 1)},
		},
	}
	e.sendBatch(t, batch1, []committerpb.Status{committerpb.Status_COMMITTED, committerpb.Status_COMMITTED})

	batch2 := &servicepb.VcBatch{
		Transactions: []*servicepb.VcTx{
			{Ref: committerpb.NewTxRef("order-tx-3", 2, 0)},
		},
	}
	e.sendBatch(t, batch2, []committerpb.Status{committerpb.Status_COMMITTED})

	require.Equal(t, []string{"order-tx-1", "order-tx-2", "order-tx-3"}, e.vc.GetReceivedTxOrder())

	// Faulty-dropped TXs are not recorded (they never reach process()).
	e.vc.MockFaultyNodeDropSize = 1
	batch3 := &servicepb.VcBatch{
		Transactions: []*servicepb.VcTx{
			{Ref: committerpb.NewTxRef("order-tx-dropped", 3, 0)},
			{Ref: committerpb.NewTxRef("order-tx-4", 3, 1)},
		},
	}
	require.NoError(t, e.streams[0].Send(batch3))
	statusBatch, err := e.streams[0].Recv()
	require.NoError(t, err)
	require.Len(t, statusBatch.Status, 1)
	require.Equal(t, "order-tx-4", statusBatch.Status[0].Ref.TxId)

	require.Equal(t, []string{"order-tx-1", "order-tx-2", "order-tx-3", "order-tx-4"}, e.vc.GetReceivedTxOrder())
}

// TestVcServiceFaultyNodeSimulation tests the faulty node simulation feature.
func TestVcServiceFaultyNodeSimulation(t *testing.T) {
	t.Parallel()

	e := startVCTestEnv(t, test.StartServerParameters{NumService: 1})
	e.vc.MockFaultyNodeDropSize = 2 // Drop first 2 transactions

	// Send batch with 4 transactions
	batch := &servicepb.VcBatch{
		Transactions: []*servicepb.VcTx{
			{
				Ref: committerpb.NewTxRef("faulty-tx-1", 1, 0),
				Namespaces: []*applicationpb.TxNamespace{
					{NsId: testNS, BlindWrites: []*applicationpb.Write{{Key: []byte("k1"), Value: []byte("v1")}}},
				},
			},
			{
				Ref: committerpb.NewTxRef("faulty-tx-2", 2, 1),
				Namespaces: []*applicationpb.TxNamespace{
					{NsId: testNS, BlindWrites: []*applicationpb.Write{{Key: []byte("k2"), Value: []byte("v2")}}},
				},
			},
			{
				Ref: committerpb.NewTxRef("faulty-tx-3", 3, 2),
				Namespaces: []*applicationpb.TxNamespace{
					{NsId: testNS, BlindWrites: []*applicationpb.Write{{Key: []byte("k3"), Value: []byte("v3")}}},
				},
			},
			{
				Ref: committerpb.NewTxRef("faulty-tx-4", 4, 3),
				Namespaces: []*applicationpb.TxNamespace{
					{NsId: testNS, BlindWrites: []*applicationpb.Write{{Key: []byte("k4"), Value: []byte("v4")}}},
				},
			},
		},
	}

	err := e.streams[0].Send(batch)
	require.NoError(t, err)

	// Should only receive status for last 2 transactions
	statusBatch, err := e.streams[0].Recv()
	require.NoError(t, err)
	require.Len(t, statusBatch.Status, 2)
	require.Equal(t, "faulty-tx-3", statusBatch.Status[0].Ref.TxId)
	require.Equal(t, "faulty-tx-4", statusBatch.Status[1].Ref.TxId)
}
