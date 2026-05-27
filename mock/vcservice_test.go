/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package mock

import (
	"context"
	"testing"
	"time"

	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

// TestVcService tests the mock VC service implementation through its gRPC interface.
func TestVcService(t *testing.T) {
	t.Parallel()

	vc := NewMockVcService()
	require.NotNil(t, vc)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	t.Cleanup(cancel)

	serverConfig := test.NewLocalHostServiceConfig(test.InsecureTLSConfig)
	test.ServeForTest(ctx, t, serverConfig, vc)

	conn := test.NewInsecureConnection(t, &serverConfig.GRPC.Endpoint)
	client := servicepb.NewValidationAndCommitServiceClient(conn)

	t.Run("set and get block number", func(t *testing.T) {
		t.Parallel()

		// Set last committed block number
		blockRef := &servicepb.BlockRef{
			Number: 10,
		}
		_, err := client.SetLastCommittedBlockNumber(ctx, blockRef)
		require.NoError(t, err)

		// Get next block number (should be 11)
		nextBlock, err := client.GetNextBlockNumberToCommit(ctx, nil)
		require.NoError(t, err)
		require.NotNil(t, nextBlock)
		require.Equal(t, uint64(11), nextBlock.Number)
	})

	t.Run("get namespace policies", func(t *testing.T) {
		t.Parallel()

		policies, err := client.GetNamespacePolicies(ctx, nil)
		require.NoError(t, err)
		require.NotNil(t, policies)
	})

	t.Run("get config transaction", func(t *testing.T) {
		t.Parallel()

		configTx, err := client.GetConfigTransaction(ctx, nil)
		require.NoError(t, err)
		require.NotNil(t, configTx)
	})

	t.Run("setup system tables", func(t *testing.T) {
		t.Parallel()

		_, err := client.SetupSystemTablesAndNamespaces(ctx, nil)
		require.NoError(t, err)
	})
}

// TestVcServiceStreamProcessing tests the transaction processing pipeline through streaming.
func TestVcServiceStreamProcessing(t *testing.T) {
	t.Parallel()

	t.Run("process valid transactions", func(t *testing.T) {
		t.Parallel()

		vc := NewMockVcService()
		require.NotNil(t, vc)

		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
		t.Cleanup(cancel)

		serverConfig := test.NewLocalHostServiceConfig(test.InsecureTLSConfig)
		test.ServeForTest(ctx, t, serverConfig, vc)

		conn := test.NewInsecureConnection(t, &serverConfig.GRPC.Endpoint)
		client := servicepb.NewValidationAndCommitServiceClient(conn)

		stream, err := client.StartValidateAndCommitStream(ctx)
		require.NoError(t, err)

		// Send batch of transactions
		batch := &servicepb.VcBatch{
			Transactions: []*servicepb.VcTx{
				{
					Ref: committerpb.NewTxRef("tx1", 1, 0),
					Namespaces: []*applicationpb.TxNamespace{
						{
							NsId: "ns1",
							BlindWrites: []*applicationpb.Write{
								{Key: []byte("key1"), Value: []byte("value1")},
							},
						},
					},
				},
				{
					Ref: committerpb.NewTxRef("tx2", 2, 0),
					Namespaces: []*applicationpb.TxNamespace{
						{
							NsId: "ns1",
							BlindWrites: []*applicationpb.Write{
								{Key: []byte("key2"), Value: []byte("value2")},
							},
						},
					},
				},
			},
		}

		err = stream.Send(batch)
		require.NoError(t, err)

		// Receive status
		statusBatch, err := stream.Recv()
		require.NoError(t, err)
		require.NotNil(t, statusBatch)
		require.Len(t, statusBatch.Status, 2)
		require.Equal(t, committerpb.Status_COMMITTED, statusBatch.Status[0].Status)
		require.Equal(t, "tx1", statusBatch.Status[0].Ref.TxId)
		require.Equal(t, committerpb.Status_COMMITTED, statusBatch.Status[1].Status)
		require.Equal(t, "tx2", statusBatch.Status[1].Ref.TxId)

		// Verify batch counter
		require.Equal(t, uint32(1), vc.NumBatchesReceived.Load())
	})

	t.Run("process transactions with preliminary invalid status", func(t *testing.T) {
		t.Parallel()

		vc := NewMockVcService()
		require.NotNil(t, vc)

		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
		t.Cleanup(cancel)

		serverConfig := test.NewLocalHostServiceConfig(test.InsecureTLSConfig)
		test.ServeForTest(ctx, t, serverConfig, vc)

		conn := test.NewInsecureConnection(t, &serverConfig.GRPC.Endpoint)
		client := servicepb.NewValidationAndCommitServiceClient(conn)

		stream, err := client.StartValidateAndCommitStream(ctx)
		require.NoError(t, err)

		invalidStatus := committerpb.Status_ABORTED_MVCC_CONFLICT
		batch := &servicepb.VcBatch{
			Transactions: []*servicepb.VcTx{
				{
					Ref:                   committerpb.NewTxRef("tx-invalid", 10, 0),
					PrelimInvalidTxStatus: &invalidStatus,
					Namespaces: []*applicationpb.TxNamespace{
						{
							NsId: "ns1",
							BlindWrites: []*applicationpb.Write{
								{Key: []byte("key3"), Value: []byte("value3")},
							},
						},
					},
				},
			},
		}

		err = stream.Send(batch)
		require.NoError(t, err)

		statusBatch, err := stream.Recv()
		require.NoError(t, err)
		require.NotNil(t, statusBatch)
		require.Len(t, statusBatch.Status, 1)
		require.Equal(t, committerpb.Status_ABORTED_MVCC_CONFLICT, statusBatch.Status[0].Status)
		require.Equal(t, "tx-invalid", statusBatch.Status[0].Ref.TxId)
	})

	t.Run("multiple batches processing", func(t *testing.T) {
		t.Parallel()

		vc := NewMockVcService()
		require.NotNil(t, vc)

		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
		t.Cleanup(cancel)

		serverConfig := test.NewLocalHostServiceConfig(test.InsecureTLSConfig)
		test.ServeForTest(ctx, t, serverConfig, vc)

		conn := test.NewInsecureConnection(t, &serverConfig.GRPC.Endpoint)
		client := servicepb.NewValidationAndCommitServiceClient(conn)

		stream, err := client.StartValidateAndCommitStream(ctx)
		require.NoError(t, err)

		// Send multiple batches
		for i := range 3 {
			batch := &servicepb.VcBatch{
				Transactions: []*servicepb.VcTx{
					{
						//nolint:gosec // integer overflow conversion int -> uint64
						Ref: committerpb.NewTxRef("batch-tx-"+string(rune(i)), uint64(100+i), 0),
						Namespaces: []*applicationpb.TxNamespace{
							{
								NsId: "ns1",
								BlindWrites: []*applicationpb.Write{
									{Key: []byte("batch-key"), Value: []byte("batch-value")},
								},
							},
						},
					},
				},
			}

			err = stream.Send(batch)
			require.NoError(t, err)

			statusBatch, err := stream.Recv()
			require.NoError(t, err)
			require.NotNil(t, statusBatch)
			require.Len(t, statusBatch.Status, 1)
			require.Equal(t, committerpb.Status_COMMITTED, statusBatch.Status[0].Status)
		}
	})
}

// TestVcServiceMVCCValidation tests MVCC validation logic.
func TestVcServiceMVCCValidation(t *testing.T) {
	t.Parallel()

	t.Run("mvcc conflict detection", func(t *testing.T) {
		t.Parallel()

		vc := NewMockVcService()
		require.NotNil(t, vc)
		vc.FullMVCC.Store(true) // Enable full MVCC validation

		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
		t.Cleanup(cancel)

		serverConfig := test.NewLocalHostServiceConfig(test.InsecureTLSConfig)
		test.ServeForTest(ctx, t, serverConfig, vc)

		conn := test.NewInsecureConnection(t, &serverConfig.GRPC.Endpoint)
		client := servicepb.NewValidationAndCommitServiceClient(conn)

		stream, err := client.StartValidateAndCommitStream(ctx)
		require.NoError(t, err)

		// First transaction writes a key
		version := uint64(0)
		batch1 := &servicepb.VcBatch{
			Transactions: []*servicepb.VcTx{
				{
					Ref: committerpb.NewTxRef("tx-write", 1, 0),
					Namespaces: []*applicationpb.TxNamespace{
						{
							NsId: "ns1",
							ReadWrites: []*applicationpb.ReadWrite{
								{Key: []byte("conflict-key"), Value: []byte("value1"), Version: &version},
							},
						},
					},
				},
			},
		}

		err = stream.Send(batch1)
		require.NoError(t, err)

		statusBatch1, err := stream.Recv()
		require.NoError(t, err)
		require.Len(t, statusBatch1.Status, 1)
		require.Equal(t, committerpb.Status_COMMITTED, statusBatch1.Status[0].Status)

		// Second transaction tries to read with old version (should conflict)
		batch2 := &servicepb.VcBatch{
			Transactions: []*servicepb.VcTx{
				{
					Ref: committerpb.NewTxRef("tx-conflict", 2, 0),
					Namespaces: []*applicationpb.TxNamespace{
						{
							NsId: "ns1",
							ReadsOnly: []*applicationpb.Read{
								{Key: []byte("conflict-key"), Version: &version},
							},
						},
					},
				},
			},
		}

		err = stream.Send(batch2)
		require.NoError(t, err)

		statusBatch2, err := stream.Recv()
		require.NoError(t, err)
		require.Len(t, statusBatch2.Status, 1)
		require.Equal(t, committerpb.Status_ABORTED_MVCC_CONFLICT, statusBatch2.Status[0].Status)
	})

	t.Run("duplicate transaction id detection", func(t *testing.T) {
		t.Parallel()

		vc := NewMockVcService()
		require.NotNil(t, vc)
		vc.FullMVCC.Store(true) // Enable full MVCC validation

		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
		t.Cleanup(cancel)

		serverConfig := test.NewLocalHostServiceConfig(test.InsecureTLSConfig)
		test.ServeForTest(ctx, t, serverConfig, vc)

		conn := test.NewInsecureConnection(t, &serverConfig.GRPC.Endpoint)
		client := servicepb.NewValidationAndCommitServiceClient(conn)

		stream, err := client.StartValidateAndCommitStream(ctx)
		require.NoError(t, err)

		// First transaction
		batch1 := &servicepb.VcBatch{
			Transactions: []*servicepb.VcTx{
				{
					Ref: committerpb.NewTxRef("duplicate-tx", 100, 0),
					Namespaces: []*applicationpb.TxNamespace{
						{
							NsId: "ns1",
							BlindWrites: []*applicationpb.Write{
								{Key: []byte("dup-key"), Value: []byte("value1")},
							},
						},
					},
				},
			},
		}

		err = stream.Send(batch1)
		require.NoError(t, err)

		statusBatch1, err := stream.Recv()
		require.NoError(t, err)
		require.Len(t, statusBatch1.Status, 1)
		require.Equal(t, committerpb.Status_COMMITTED, statusBatch1.Status[0].Status)

		// Second transaction with same ID but different ref (should be rejected)
		batch2 := &servicepb.VcBatch{
			Transactions: []*servicepb.VcTx{
				{
					Ref: committerpb.NewTxRef("duplicate-tx", 101, 0),
					Namespaces: []*applicationpb.TxNamespace{
						{
							NsId: "ns1",
							BlindWrites: []*applicationpb.Write{
								{Key: []byte("dup-key"), Value: []byte("value2")},
							},
						},
					},
				},
			},
		}

		err = stream.Send(batch2)
		require.NoError(t, err)

		statusBatch2, err := stream.Recv()
		require.NoError(t, err)
		require.Len(t, statusBatch2.Status, 1)
		require.Equal(t, committerpb.Status_REJECTED_DUPLICATE_TX_ID, statusBatch2.Status[0].Status)
	})

	t.Run("idempotent transaction processing", func(t *testing.T) {
		t.Parallel()

		vc := NewMockVcService()
		require.NotNil(t, vc)
		vc.FullMVCC.Store(true) // Enable full MVCC validation

		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
		t.Cleanup(cancel)

		serverConfig := test.NewLocalHostServiceConfig(test.InsecureTLSConfig)
		test.ServeForTest(ctx, t, serverConfig, vc)

		conn := test.NewInsecureConnection(t, &serverConfig.GRPC.Endpoint)
		client := servicepb.NewValidationAndCommitServiceClient(conn)

		stream, err := client.StartValidateAndCommitStream(ctx)
		require.NoError(t, err)

		// First transaction
		batch1 := &servicepb.VcBatch{
			Transactions: []*servicepb.VcTx{
				{
					Ref: committerpb.NewTxRef("idempotent-tx", 200, 0),
					Namespaces: []*applicationpb.TxNamespace{
						{
							NsId: "ns1",
							BlindWrites: []*applicationpb.Write{
								{Key: []byte("idem-key"), Value: []byte("value1")},
							},
						},
					},
				},
			},
		}

		err = stream.Send(batch1)
		require.NoError(t, err)

		statusBatch1, err := stream.Recv()
		require.NoError(t, err)
		require.Len(t, statusBatch1.Status, 1)
		require.Equal(t, committerpb.Status_COMMITTED, statusBatch1.Status[0].Status)

		// Same transaction again (should return same status)
		err = stream.Send(batch1)
		require.NoError(t, err)

		statusBatch2, err := stream.Recv()
		require.NoError(t, err)
		require.Len(t, statusBatch2.Status, 1)
		require.Equal(t, committerpb.Status_COMMITTED, statusBatch2.Status[0].Status)
	})

	t.Run("duplicate transaction number detection", func(t *testing.T) {
		t.Parallel()

		vc := NewMockVcService()
		require.NotNil(t, vc)

		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
		t.Cleanup(cancel)

		serverConfig := test.NewLocalHostServiceConfig(test.InsecureTLSConfig)
		test.ServeForTest(ctx, t, serverConfig, vc)

		conn := test.NewInsecureConnection(t, &serverConfig.GRPC.Endpoint)
		client := servicepb.NewValidationAndCommitServiceClient(conn)

		stream, err := client.StartValidateAndCommitStream(ctx)
		require.NoError(t, err)

		// Send batch with duplicate tx numbers
		batch := &servicepb.VcBatch{
			Transactions: []*servicepb.VcTx{
				{
					Ref: committerpb.NewTxRef("tx1", 1, 0),
					Namespaces: []*applicationpb.TxNamespace{
						{
							NsId: "ns1",
							BlindWrites: []*applicationpb.Write{
								{Key: []byte("key1"), Value: []byte("value1")},
							},
						},
					},
				},
				{
					Ref: committerpb.NewTxRef("tx2", 1, 0), // Duplicate tx number
					Namespaces: []*applicationpb.TxNamespace{
						{
							NsId: "ns1",
							BlindWrites: []*applicationpb.Write{
								{Key: []byte("key2"), Value: []byte("value2")},
							},
						},
					},
				},
			},
		}

		err = stream.Send(batch)
		require.NoError(t, err)

		// Should receive error due to duplicate tx num
		_, err = stream.Recv()
		require.Error(t, err)
		require.ErrorContains(t, err, "duplication tx num detected")
	})
}

// TestVcServiceGetTransactionsStatus tests transaction status retrieval.
func TestVcServiceGetTransactionsStatus(t *testing.T) {
	t.Parallel()

	vc := NewMockVcService()
	require.NotNil(t, vc)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	t.Cleanup(cancel)

	serverConfig := test.NewLocalHostServiceConfig(test.InsecureTLSConfig)
	test.ServeForTest(ctx, t, serverConfig, vc)

	conn := test.NewInsecureConnection(t, &serverConfig.GRPC.Endpoint)
	client := servicepb.NewValidationAndCommitServiceClient(conn)

	// Process some transactions first
	stream, err := client.StartValidateAndCommitStream(ctx)
	require.NoError(t, err)

	batch := &servicepb.VcBatch{
		Transactions: []*servicepb.VcTx{
			{
				Ref: committerpb.NewTxRef("status-tx-1", 1, 0),
				Namespaces: []*applicationpb.TxNamespace{
					{
						NsId: "ns1",
						BlindWrites: []*applicationpb.Write{
							{Key: []byte("key1"), Value: []byte("value1")},
						},
					},
				},
			},
			{
				Ref: committerpb.NewTxRef("status-tx-2", 2, 0),
				Namespaces: []*applicationpb.TxNamespace{
					{
						NsId: "ns1",
						BlindWrites: []*applicationpb.Write{
							{Key: []byte("key2"), Value: []byte("value2")},
						},
					},
				},
			},
		},
	}

	err = stream.Send(batch)
	require.NoError(t, err)

	_, err = stream.Recv()
	require.NoError(t, err)

	// Query transaction status
	query := &committerpb.TxIDsBatch{
		TxIds: []string{"status-tx-1", "status-tx-2", "non-existent-tx"},
	}

	statusBatch, err := client.GetTransactionsStatus(ctx, query)
	require.NoError(t, err)
	require.NotNil(t, statusBatch)
	require.Len(t, statusBatch.Status, 3)

	// First two should have status
	require.NotNil(t, statusBatch.Status[0])
	require.Equal(t, "status-tx-1", statusBatch.Status[0].Ref.TxId)
	require.Equal(t, committerpb.Status_COMMITTED, statusBatch.Status[0].Status)

	require.NotNil(t, statusBatch.Status[1])
	require.Equal(t, "status-tx-2", statusBatch.Status[1].Ref.TxId)
	require.Equal(t, committerpb.Status_COMMITTED, statusBatch.Status[1].Status)

	// Third should be nil (not found)
	require.Nil(t, statusBatch.Status[2])
}

// TestVcServiceMultipleStreams tests multiple concurrent streams.
func TestVcServiceMultipleStreams(t *testing.T) {
	t.Parallel()

	vc := NewMockVcService()
	require.NotNil(t, vc)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	t.Cleanup(cancel)

	numServices := 3
	sc := test.ServeManyForTest(ctx, t, test.StartServerParameters{
		NumService: numServices,
	}, vc)
	require.NotNil(t, sc)
	require.Len(t, sc.Configs, numServices)

	// Verify no streams initially
	RequireStreams(t, vc, 0)

	// Create connections and streams
	streams := make([]servicepb.ValidationAndCommitService_StartValidateAndCommitStreamClient, numServices)
	for i := range numServices {
		conn := test.NewInsecureConnection(t, &sc.Configs[i].GRPC.Endpoint)
		client := servicepb.NewValidationAndCommitServiceClient(conn)
		stream, err := client.StartValidateAndCommitStream(ctx)
		require.NoError(t, err)
		streams[i] = stream
	}

	// Verify streams are registered
	RequireStreams(t, vc, numServices)

	// Send transactions on each stream
	for i, stream := range streams {
		batch := &servicepb.VcBatch{
			Transactions: []*servicepb.VcTx{{
				//nolint:gosec // integer overflow conversion int -> uint64
				Ref: committerpb.NewTxRef("multi-stream-tx-"+string(rune(i)), uint64(i+1), 0),
				Namespaces: []*applicationpb.TxNamespace{
					{
						NsId: "ns1",
						BlindWrites: []*applicationpb.Write{
							{Key: []byte("key"), Value: []byte("value")},
						},
					},
				},
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

// TestVcServiceWorldState tests world state management.
func TestVcServiceWorldState(t *testing.T) {
	t.Parallel()

	vc := NewMockVcService()
	require.NotNil(t, vc)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	t.Cleanup(cancel)

	serverConfig := test.NewLocalHostServiceConfig(test.InsecureTLSConfig)
	test.ServeForTest(ctx, t, serverConfig, vc)

	conn := test.NewInsecureConnection(t, &serverConfig.GRPC.Endpoint)
	client := servicepb.NewValidationAndCommitServiceClient(conn)

	stream, err := client.StartValidateAndCommitStream(ctx)
	require.NoError(t, err)

	// Write some keys
	batch := &servicepb.VcBatch{
		Transactions: []*servicepb.VcTx{
			{
				Ref: committerpb.NewTxRef("world-state-tx", 1, 0),
				Namespaces: []*applicationpb.TxNamespace{
					{
						NsId: "test-ns",
						BlindWrites: []*applicationpb.Write{
							{Key: []byte("ws-key1"), Value: []byte("ws-value1")},
							{Key: []byte("ws-key2"), Value: []byte("ws-value2")},
						},
					},
				},
			},
		},
	}

	err = stream.Send(batch)
	require.NoError(t, err)

	_, err = stream.Recv()
	require.NoError(t, err)

	// Verify world state
	worldState := vc.GetKeys("test-ns", []byte("ws-key1"), []byte("ws-key2"), []byte("non-existent"))
	require.Len(t, worldState, 2)

	require.Equal(t, "test-ns", worldState[0].Namespace)
	require.Equal(t, []byte("ws-key1"), worldState[0].Key)
	require.Equal(t, []byte("ws-value1"), worldState[0].Value)
	require.Equal(t, uint64(0), worldState[0].Version)

	require.Equal(t, "test-ns", worldState[1].Namespace)
	require.Equal(t, []byte("ws-key2"), worldState[1].Key)
	require.Equal(t, []byte("ws-value2"), worldState[1].Value)
	require.Equal(t, uint64(0), worldState[1].Version)
}

// TestVcServiceFaultyNodeSimulation tests the faulty node simulation feature.
func TestVcServiceFaultyNodeSimulation(t *testing.T) {
	t.Parallel()

	vc := NewMockVcService()
	require.NotNil(t, vc)
	vc.MockFaultyNodeDropSize = 2 // Drop first 2 transactions

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	t.Cleanup(cancel)

	serverConfig := test.NewLocalHostServiceConfig(test.InsecureTLSConfig)
	test.ServeForTest(ctx, t, serverConfig, vc)

	conn := test.NewInsecureConnection(t, &serverConfig.GRPC.Endpoint)
	client := servicepb.NewValidationAndCommitServiceClient(conn)

	stream, err := client.StartValidateAndCommitStream(ctx)
	require.NoError(t, err)

	// Send batch with 4 transactions
	batch := &servicepb.VcBatch{
		Transactions: []*servicepb.VcTx{
			{
				Ref: committerpb.NewTxRef("faulty-tx-1", 1, 0),
				Namespaces: []*applicationpb.TxNamespace{
					{NsId: "ns1", BlindWrites: []*applicationpb.Write{{Key: []byte("k1"), Value: []byte("v1")}}},
				},
			},
			{
				Ref: committerpb.NewTxRef("faulty-tx-2", 2, 0),
				Namespaces: []*applicationpb.TxNamespace{
					{NsId: "ns1", BlindWrites: []*applicationpb.Write{{Key: []byte("k2"), Value: []byte("v2")}}},
				},
			},
			{
				Ref: committerpb.NewTxRef("faulty-tx-3", 3, 0),
				Namespaces: []*applicationpb.TxNamespace{
					{NsId: "ns1", BlindWrites: []*applicationpb.Write{{Key: []byte("k3"), Value: []byte("v3")}}},
				},
			},
			{
				Ref: committerpb.NewTxRef("faulty-tx-4", 4, 0),
				Namespaces: []*applicationpb.TxNamespace{
					{NsId: "ns1", BlindWrites: []*applicationpb.Write{{Key: []byte("k4"), Value: []byte("v4")}}},
				},
			},
		},
	}

	err = stream.Send(batch)
	require.NoError(t, err)

	// Should only receive status for last 2 transactions
	statusBatch, err := stream.Recv()
	require.NoError(t, err)
	require.Len(t, statusBatch.Status, 2)
	require.Equal(t, "faulty-tx-3", statusBatch.Status[0].Ref.TxId)
	require.Equal(t, "faulty-tx-4", statusBatch.Status[1].Ref.TxId)
}

// Made with Bob
