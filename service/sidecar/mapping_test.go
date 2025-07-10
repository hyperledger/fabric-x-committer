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
	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/utils/logging"
	"github.com/hyperledger/fabric-x-committer/utils/serialization"
)

func BenchmarkMapBlock(b *testing.B) {
	logging.SetupWithConfig(&logging.Config{Enabled: false})
	txs := workload.GenerateTransactions(b, workload.DefaultProfile(8), b.N)
	block := makeBlock(b, txs)

	var mappedBlock *scBlockWithStatus
	b.ResetTimer()
	mappedBlock = mapBlock(block)
	b.StopTimer()
	require.NotNil(b, mappedBlock)
}

func TestBlockMapping(t *testing.T) {
	t.Parallel()
	txs := make([]*protoblocktx.Tx, len(MalformedTxTestCases))
	expectedStatus := make([]validationCode, len(MalformedTxTestCases))
	expectedBlockSize := 0
	for i, tt := range MalformedTxTestCases {
		txs[i] = tt.Tx
		expectedStatus[i] = validationCode(tt.ExpectedStatus)
		if tt.ExpectedStatus == statusNotYetValidated {
			expectedBlockSize++
		}
	}
	block := makeBlock(t, txs)
	mappedBlock := mapBlock(block)

	require.NotNil(t, mappedBlock)
	require.NotNil(t, mappedBlock.block)
	require.NotNil(t, mappedBlock.withStatus)

	require.Equal(t, block, mappedBlock.withStatus.block)
	require.Equal(t, block.Header.Number, mappedBlock.block.Number)
	require.Equal(t, expectedStatus, mappedBlock.withStatus.txStatus)

	require.Len(t, mappedBlock.withStatus.txIDToTxNum, expectedBlockSize)
	require.Len(t, mappedBlock.block.Txs, expectedBlockSize)
	require.Len(t, mappedBlock.block.TxsNum, expectedBlockSize)
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
