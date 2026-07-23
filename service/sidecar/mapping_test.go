/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package sidecar

import (
	"fmt"
	"testing"

	"github.com/hyperledger/fabric-lib-go/common/flogging"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/utils"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

// testChannelID is a shared channel ID used by sidecar mapping/relay tests.
const testChannelID = "chan"

func BenchmarkMapOneBlock(b *testing.B) {
	flogging.ActivateSpec("fatal")
	txs := workload.GenerateTransactions(b, nil, b.N)
	block := workload.MapToOrdererBlock(1, txs)

	var txIDToHeight utils.SyncMap[string, servicepb.Height]
	b.ResetTimer()
	mappedBlock, err := mapBlock(block, &txIDToHeight)
	b.StopTimer()
	test.ReportTxPerSecond(b)
	require.NoError(b, err, "This can never occur unless there is a bug in the relay.")
	require.NotNil(b, mappedBlock)
}

func BenchmarkMapBlockSize(b *testing.B) {
	flogging.ActivateSpec("fatal")
	for _, blockSize := range []int{100, 1000, 5000, 10000} {
		b.Run(fmt.Sprintf("blockSize=%d", blockSize), func(b *testing.B) {
			// b.N is the number of transactions; blockSize is only the work
			// granularity. We split b.N transactions into blocks of at most
			// blockSize (the final block may be smaller), so ns/op and tx/s are
			// reported per transaction, independent of the block size.
			allTxs := workload.GenerateTransactions(b, nil, b.N)
			blocks := make([]*common.Block, 0, (b.N+blockSize-1)/blockSize)
			for off := 0; off < b.N; off += blockSize {
				blocks = append(blocks, workload.MapToOrdererBlock(
					uint64(len(blocks)), allTxs[off:min(off+blockSize, b.N)],
				))
			}

			b.ResetTimer()
			for _, blk := range blocks {
				var txIDToHeight utils.SyncMap[string, servicepb.Height]
				if _, err := mapBlock(blk, &txIDToHeight); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			test.ReportTxPerSecond(b)
		})
	}
}

func TestBlockMapping(t *testing.T) {
	t.Parallel()
	txb := &workload.TxBuilder{ChannelID: testChannelID}
	txs, expected := MalformedTxTestCases(txb)
	expectedBlockSize := 0
	expectedRejected := 0
	for i, e := range expected {
		if !IsStatusStoredInDB(e) {
			continue
		}
		expected[i] = statusNotYetValidated
		expectedBlockSize++
		if e != committerpb.Status_COMMITTED {
			expectedRejected++
		}
	}
	lgTX := txb.MakeTx(txs[0].Tx)
	txs = append(txs, lgTX)
	expected = append(expected, committerpb.Status_REJECTED_DUPLICATE_TX_ID)

	var txIDToHeight utils.SyncMap[string, servicepb.Height]
	txIDToHeight.Store(lgTX.Id, servicepb.Height{})

	block := workload.MapToOrdererBlock(1, txs)
	mappedBlock, err := mapBlock(block, &txIDToHeight)
	require.NoError(t, err, "This can never occur unless there is a bug in the relay.")

	require.NotNil(t, mappedBlock)
	require.NotNil(t, mappedBlock.block)
	require.NotNil(t, mappedBlock.withStatus)

	require.Equal(t, block, mappedBlock.withStatus.block)
	require.Equal(t, block.Header.Number, mappedBlock.blockNumber)
	require.Equal(t, expected, mappedBlock.withStatus.txStatus)

	require.Equal(t, expectedBlockSize+1, txIDToHeight.Count())
	require.Len(t, mappedBlock.block.Txs, expectedBlockSize-expectedRejected)
	require.Len(t, mappedBlock.block.Rejected, expectedRejected)
	//nolint:gosec // int -> int32
	require.Equal(t, int32(expectedBlockSize), mappedBlock.withStatus.pendingCount.Load())
}

func TestSystemNamespaceFormValidation(t *testing.T) {
	t.Parallel()

	const ordinaryNsID = "ordinary"
	heightKey := servicepb.NewHeight(7, 3).ToBytes()
	heightKeyWithTrailingBytes := append(append([]byte{}, heightKey...), []byte("junk")...)

	for _, tc := range []struct {
		name                string
		tx                  *applicationpb.Tx
		expectedStatus      committerpb.Status
		expectedHasSnapshot bool
		expectedSnapshotIdx int
	}{
		{
			name: "marker-only snapshot namespace is valid",
			tx: &applicationpb.Tx{
				Namespaces:   []*applicationpb.TxNamespace{{NsId: committerpb.SnapshotNamespaceID}},
				Endorsements: dummyEndorsements(1),
			},
			expectedStatus:      statusNotYetValidated,
			expectedHasSnapshot: true,
			expectedSnapshotIdx: 0,
		},
		{
			name: "snapshot namespace with reads-only is malformed",
			tx: &applicationpb.Tx{
				Namespaces: []*applicationpb.TxNamespace{{
					NsId:      committerpb.SnapshotNamespaceID,
					ReadsOnly: []*applicationpb.Read{{Key: []byte("key")}},
				}},
				Endorsements: dummyEndorsements(1),
			},
			expectedStatus: committerpb.Status_MALFORMED_SNAPSHOT_NOT_MARKER_ONLY,
		},
		{
			name: "snapshot namespace with read-writes is malformed",
			tx: &applicationpb.Tx{
				Namespaces: []*applicationpb.TxNamespace{{
					NsId:       committerpb.SnapshotNamespaceID,
					ReadWrites: []*applicationpb.ReadWrite{{Key: []byte("key"), Value: []byte("value")}},
				}},
				Endorsements: dummyEndorsements(1),
			},
			expectedStatus: committerpb.Status_MALFORMED_SNAPSHOT_NOT_MARKER_ONLY,
		},
		{
			name: "snapshot namespace with blind-writes is malformed",
			tx: &applicationpb.Tx{
				Namespaces: []*applicationpb.TxNamespace{{
					NsId:        committerpb.SnapshotNamespaceID,
					BlindWrites: []*applicationpb.Write{{Key: []byte("key"), Value: []byte("value")}},
				}},
				Endorsements: dummyEndorsements(1),
			},
			expectedStatus: committerpb.Status_MALFORMED_SNAPSHOT_NOT_MARKER_ONLY,
		},
		{
			name: "snapshot namespace mixed with ordinary namespace is malformed",
			tx: &applicationpb.Tx{
				Namespaces: []*applicationpb.TxNamespace{
					{NsId: committerpb.SnapshotNamespaceID},
					{NsId: ordinaryNsID, BlindWrites: []*applicationpb.Write{{Key: []byte("key")}}},
				},
				Endorsements: dummyEndorsements(2),
			},
			expectedStatus: committerpb.Status_MALFORMED_SYSTEM_TX_NOT_STANDALONE,
		},
		{
			name: "ordinary empty namespace is malformed with no writes",
			tx: &applicationpb.Tx{
				Namespaces:   []*applicationpb.TxNamespace{{NsId: ordinaryNsID}},
				Endorsements: dummyEndorsements(1),
			},
			expectedStatus: committerpb.Status_MALFORMED_NO_WRITES,
		},
		{
			name: "checkpoint namespace with height key is valid",
			tx: &applicationpb.Tx{
				Namespaces: []*applicationpb.TxNamespace{{
					NsId:       committerpb.CheckpointNamespaceID,
					ReadWrites: []*applicationpb.ReadWrite{{Key: heightKey, Value: []byte("checkpoint")}},
				}},
				Endorsements: dummyEndorsements(1),
			},
			expectedStatus: statusNotYetValidated,
		},
		{
			name: "checkpoint namespace with non-height key is malformed",
			tx: &applicationpb.Tx{
				Namespaces: []*applicationpb.TxNamespace{{
					NsId:       committerpb.CheckpointNamespaceID,
					ReadWrites: []*applicationpb.ReadWrite{{Key: []byte("not-height"), Value: []byte("checkpoint")}},
				}},
				Endorsements: dummyEndorsements(1),
			},
			expectedStatus: committerpb.Status_MALFORMED_CHECKPOINT_INVALID_KEY,
		},
		{
			name: "checkpoint namespace with height key plus trailing bytes is malformed",
			tx: &applicationpb.Tx{
				Namespaces: []*applicationpb.TxNamespace{{
					NsId: committerpb.CheckpointNamespaceID,
					ReadWrites: []*applicationpb.ReadWrite{{
						Key:   heightKeyWithTrailingBytes,
						Value: []byte("checkpoint"),
					}},
				}},
				Endorsements: dummyEndorsements(1),
			},
			expectedStatus: committerpb.Status_MALFORMED_CHECKPOINT_INVALID_KEY,
		},
		{
			name: "checkpoint namespace mixed with ordinary namespace is malformed",
			tx: &applicationpb.Tx{
				Namespaces: []*applicationpb.TxNamespace{
					{
						NsId:       committerpb.CheckpointNamespaceID,
						ReadWrites: []*applicationpb.ReadWrite{{Key: heightKey, Value: []byte("checkpoint")}},
					},
					{NsId: ordinaryNsID, BlindWrites: []*applicationpb.Write{{Key: []byte("key")}}},
				},
				Endorsements: dummyEndorsements(2),
			},
			expectedStatus: committerpb.Status_MALFORMED_SYSTEM_TX_NOT_STANDALONE,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tc.expectedStatus, verifyTxForm(tc.tx))

			txb := &workload.TxBuilder{ChannelID: testChannelID}
			block := workload.MapToOrdererBlock(1, []*servicepb.LoadGenTx{txb.MakeTx(tc.tx)})

			var txIDToHeight utils.SyncMap[string, servicepb.Height]
			mappedBlock, err := mapBlock(block, &txIDToHeight)
			require.NoError(t, err)
			require.NotNil(t, mappedBlock)
			require.Equal(t, tc.expectedHasSnapshot, mappedBlock.hasSnapshot)
			require.Equal(t, tc.expectedSnapshotIdx, mappedBlock.snapshotTxIndex)
		})
	}
}

// TestDuplicateSnapshotInBlock verifies that when a block contains more than one snapshot TX,
// only the first is accepted and the rest are rejected with
// REJECTED_DUPLICATE_SNAPSHOT_IN_BLOCK (a stored status), regardless of the first's outcome.
func TestDuplicateSnapshotInBlock(t *testing.T) {
	t.Parallel()

	snapshotTx := func() *applicationpb.Tx {
		return &applicationpb.Tx{
			Namespaces:   []*applicationpb.TxNamespace{{NsId: committerpb.SnapshotNamespaceID}},
			Endorsements: dummyEndorsements(1),
		}
	}
	regularTx := func() *applicationpb.Tx {
		return &applicationpb.Tx{
			Namespaces: []*applicationpb.TxNamespace{{
				NsId:        "ns",
				BlindWrites: []*applicationpb.Write{{Key: []byte("key")}},
			}},
			Endorsements: dummyEndorsements(1),
		}
	}

	txb := &workload.TxBuilder{ChannelID: testChannelID}
	// Block layout: [regular, snapshot#0 (accepted), regular, snapshot#1 (rejected)].
	block := workload.MapToOrdererBlock(1, []*servicepb.LoadGenTx{
		txb.MakeTx(regularTx()),
		txb.MakeTx(snapshotTx()),
		txb.MakeTx(regularTx()),
		txb.MakeTx(snapshotTx()),
	})

	var txIDToHeight utils.SyncMap[string, servicepb.Height]
	mappedBlock, err := mapBlock(block, &txIDToHeight)
	require.NoError(t, err)
	require.NotNil(t, mappedBlock)

	// Only the first snapshot is accepted; its index points into the accepted Txs slice
	// (regular#0 + snapshot#0 + regular#2 = indexes 0,1,2), so the snapshot is at index 1.
	require.True(t, mappedBlock.hasSnapshot)
	require.Equal(t, 1, mappedBlock.snapshotTxIndex)
	require.Len(t, mappedBlock.block.Txs, 3)

	// The second snapshot is rejected with the dedicated stored status.
	require.Len(t, mappedBlock.block.Rejected, 1)
	require.Equal(
		t,
		committerpb.Status_REJECTED_DUPLICATE_SNAPSHOT_IN_BLOCK,
		mappedBlock.block.Rejected[0].Status,
	)
	require.Equal(t, uint32(3), mappedBlock.block.Rejected[0].Ref.TxNum)
}
