/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package sidecar

import (
	"testing"

	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/utils"
	"github.com/hyperledger/fabric-x-committer/utils/logging"
)

func BenchmarkMapBlock(b *testing.B) {
	logging.SetupWithConfig(&logging.Config{Enabled: false})
	txs := workload.GenerateTransactions(b, workload.DefaultProfile(8), b.N)
	block := workload.MapToOrdererBlock(1, txs)

	var txIDToHeight utils.SyncMap[string, servicepb.Height]
	b.ResetTimer()
	mappedBlock, err := mapBlock(block, &txIDToHeight)
	b.StopTimer()
	require.NoError(b, err, "This can never occur unless there is a bug in the relay.")
	require.NotNil(b, mappedBlock)
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
	require.Equal(t, expectedBlockSize, mappedBlock.withStatus.pendingCount)
}
