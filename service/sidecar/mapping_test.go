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
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/utils"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

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
			// Generate b.N total TXs so each iteration processes a unique block,
			// avoiding cache/locality effects from reusing the same block.
			// Pad to ensure at least one full block.
			totalTxs := b.N
			if totalTxs < blockSize {
				totalTxs = blockSize
			}
			allTxs := workload.GenerateTransactions(b, nil, totalTxs)

			// Pre-split into blocks of blockSize.
			numBlocks := len(allTxs) / blockSize
			blocks := make([]*common.Block, numBlocks)
			for i := range blocks {
				txSlice := allTxs[i*blockSize : (i+1)*blockSize]
				//nolint:gosec // i is a non-negative slice index
				blocks[i] = workload.MapToOrdererBlock(uint64(i), txSlice)
			}

			b.ResetTimer()
			blockIdx := 0
			for b.Loop() {
				var txIDToHeight utils.SyncMap[string, servicepb.Height]
				_, err := mapBlock(blocks[blockIdx%numBlocks], &txIDToHeight)
				if err != nil {
					b.Fatal(err)
				}
				blockIdx++
			}
			test.ReportTxPerSecond(b)
		})
	}
}

func TestBlockMapping(t *testing.T) {
	t.Parallel()
	txb := &workload.TxBuilder{ChannelID: "chan"}
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
