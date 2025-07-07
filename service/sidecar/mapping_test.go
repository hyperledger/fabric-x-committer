/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package sidecar

import (
	"testing"

	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
	"github.com/hyperledger/fabric-x-committer/api/types"
	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/utils"
	"github.com/hyperledger/fabric-x-committer/utils/logging"
	"github.com/hyperledger/fabric-x-committer/utils/serialization"
)

func BenchmarkMapBlock(b *testing.B) {
	logging.SetupWithConfig(&logging.Config{Enabled: false})
	txs := workload.GenerateTransactions(b, workload.DefaultProfile(8), b.N)
	block := makeBlock(b, txs)

	var mappedBlock *scBlockWithStatus
	var txIDToHeight utils.SyncMap[string, types.Height]
	b.ResetTimer()
	mappedBlock = mapBlock(block, &txIDToHeight)
	b.StopTimer()
	require.NotNil(b, mappedBlock)
}

func TestBlockMapping(t *testing.T) {
	t.Parallel()
	txs := make([]*protoblocktx.Tx, len(MalformedTxTestCases)+1)
	expectedStatus := make([]protoblocktx.Status, len(MalformedTxTestCases)+1)
	expectedBlockSize := 0
	expectedRejected := 0
	for i, tt := range MalformedTxTestCases {
		txs[i] = tt.Tx

		if !IsStatusStoredInDB(tt.ExpectedStatus) {
			expectedStatus[i] = tt.ExpectedStatus
			continue
		}
		expectedBlockSize++
		if tt.ExpectedStatus != statusNotYetValidated {
			expectedRejected++
		}
	}
	duplicatedID := "global duplicated ID"
	txs[len(txs)-1] = &protoblocktx.Tx{
		Id: duplicatedID,
		Namespaces: []*protoblocktx.TxNamespace{
			{
				NsId:      "1",
				NsVersion: 0,
				ReadWrites: []*protoblocktx.ReadWrite{
					{Key: []byte("k1")},
				},
			},
		},
		Signatures: make([][]byte, 1),
	}
	expectedStatus[len(txs)-1] = protoblocktx.Status_REJECTED_DUPLICATE_TX_ID
	var txIDToHeight utils.SyncMap[string, types.Height]
	txIDToHeight.Store(duplicatedID, types.Height{})

	block := makeBlock(t, txs)
	mappedBlock := mapBlock(block, &txIDToHeight)

	require.NotNil(t, mappedBlock)
	require.NotNil(t, mappedBlock.block)
	require.NotNil(t, mappedBlock.withStatus)

	require.Equal(t, block, mappedBlock.withStatus.block)
	require.Equal(t, block.Header.Number, mappedBlock.block.Number)
	require.Equal(t, expectedStatus, mappedBlock.withStatus.txStatus)

	require.Equal(t, expectedBlockSize+1, txIDToHeight.Count())
	require.Len(t, mappedBlock.block.Txs, expectedBlockSize-expectedRejected)
	require.Len(t, mappedBlock.block.TxsNum, expectedBlockSize-expectedRejected)
	require.Len(t, mappedBlock.block.Rejected, expectedRejected)
	require.Equal(t, expectedBlockSize, mappedBlock.withStatus.pendingCount)
}

func makeBlock(tb testing.TB, txs []*protoblocktx.Tx) *common.Block {
	tb.Helper()
	data := make([][]byte, len(txs))
	for i, tx := range txs {
		env, _, err := serialization.CreateEnvelope("channel", nil, tx)
		require.NoError(tb, err)
		data[i], err = proto.Marshal(env)
		require.NoError(tb, err)
	}
	return &common.Block{
		Header: &common.BlockHeader{Number: 1},
		Data:   &common.BlockData{Data: data},
	}
}
