/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package workload

import (
	"github.com/hyperledger/fabric-protos-go-apiv2/common"

	"github.com/hyperledger/fabric-x-committer/api/committerpb"
	"github.com/hyperledger/fabric-x-committer/api/servicepb"
)

// MapToCoordinatorBatch creates a Coordinator batch.
func MapToCoordinatorBatch(blockNum uint64, txs []*servicepb.LoadGenTx) *servicepb.CoordinatorBatch {
	blockTXs := make([]*servicepb.CoordinatorTx, len(txs))
	for i, tx := range txs {
		blockTXs[i] = &servicepb.CoordinatorTx{
			Ref:     makeRef(tx.Id, blockNum, i),
			Content: tx.Tx,
		}
	}
	return &servicepb.CoordinatorBatch{Txs: blockTXs}
}

// MapToVerifierBatch creates a Verifier batch.
func MapToVerifierBatch(blockNum uint64, txs []*servicepb.LoadGenTx) *servicepb.VerifierBatch {
	reqs := make([]*servicepb.VerifierTx, len(txs))
	for i, tx := range txs {
		reqs[i] = &servicepb.VerifierTx{
			Ref: makeRef(tx.Id, blockNum, i),
			Tx:  tx.Tx,
		}
	}
	return &servicepb.VerifierBatch{Requests: reqs}
}

// MapToVcBatch creates a VC batch.
func MapToVcBatch(blockNum uint64, txs []*servicepb.LoadGenTx) *servicepb.VcBatch {
	batchTxs := make([]*servicepb.VcTx, len(txs))
	for i, tx := range txs {
		batchTxs[i] = &servicepb.VcTx{
			Ref:        makeRef(tx.Id, blockNum, i),
			Namespaces: tx.Tx.Namespaces,
		}
	}
	return &servicepb.VcBatch{Transactions: batchTxs}
}

// MapToLoadGenBatch creates a load-gen batch.
func MapToLoadGenBatch(_ uint64, txs []*servicepb.LoadGenTx) *servicepb.LoadGenBatch {
	return &servicepb.LoadGenBatch{Tx: txs}
}

// MapToEnvelopeBatch creates a batch of Fabric's Orderer input envelopes.
func MapToEnvelopeBatch(_ uint64, txs []*servicepb.LoadGenTx) []*common.Envelope {
	envs := make([]*common.Envelope, len(txs))
	for i, tx := range txs {
		envs[i] = &common.Envelope{
			Payload:   tx.EnvelopePayload,
			Signature: tx.EnvelopeSignature,
		}
	}
	return envs
}

// MapToOrdererBlock creates a Fabric's Orderer output block.
func MapToOrdererBlock(blockNum uint64, txs []*servicepb.LoadGenTx) *common.Block {
	data := make([][]byte, len(txs))
	for i, tx := range txs {
		data[i] = tx.SerializedEnvelope
	}
	return &common.Block{
		Header: &common.BlockHeader{Number: blockNum},
		Data:   &common.BlockData{Data: data},
	}
}

func makeRef(txID string, blockNum uint64, txNum int) *committerpb.TxRef {
	return committerpb.NewTxRef(txID, blockNum, uint32(txNum)) //nolint:gosec // int -> uint32.
}
