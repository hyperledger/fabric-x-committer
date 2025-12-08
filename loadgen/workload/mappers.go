/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package workload

import (
	"github.com/hyperledger/fabric-protos-go-apiv2/common"

	"github.com/hyperledger/fabric-x-committer/api/committerpb"
	"github.com/hyperledger/fabric-x-committer/api/protocoordinatorservice"
	"github.com/hyperledger/fabric-x-committer/api/protoloadgen"
	"github.com/hyperledger/fabric-x-committer/api/protosigverifierservice"
	"github.com/hyperledger/fabric-x-committer/api/protovcservice"
)

// MapToCoordinatorBatch creates a Coordinator batch.
func MapToCoordinatorBatch(blockNum uint64, txs []*protoloadgen.TX) *protocoordinatorservice.Batch {
	blockTXs := make([]*protocoordinatorservice.Tx, len(txs))
	for i, tx := range txs {
		blockTXs[i] = &protocoordinatorservice.Tx{
			Ref:     makeRef(tx.Id, blockNum, i),
			Content: tx.Tx,
		}
	}
	return &protocoordinatorservice.Batch{Txs: blockTXs}
}

// MapToVerifierBatch creates a Verifier batch.
func MapToVerifierBatch(blockNum uint64, txs []*protoloadgen.TX) *protosigverifierservice.Batch {
	reqs := make([]*protosigverifierservice.Tx, len(txs))
	for i, tx := range txs {
		reqs[i] = &protosigverifierservice.Tx{
			Ref: makeRef(tx.Id, blockNum, i),
			Tx:  tx.Tx,
		}
	}
	return &protosigverifierservice.Batch{Requests: reqs}
}

// MapToVcBatch creates a VC batch.
func MapToVcBatch(blockNum uint64, txs []*protoloadgen.TX) *protovcservice.Batch {
	batchTxs := make([]*protovcservice.Tx, len(txs))
	for i, tx := range txs {
		batchTxs[i] = &protovcservice.Tx{
			Ref:        makeRef(tx.Id, blockNum, i),
			Namespaces: tx.Tx.Namespaces,
		}
	}
	return &protovcservice.Batch{Transactions: batchTxs}
}

// MapToLoadGenBatch creates a load-gen batch.
func MapToLoadGenBatch(_ uint64, txs []*protoloadgen.TX) *protoloadgen.Batch {
	return &protoloadgen.Batch{Tx: txs}
}

// MapToEnvelopeBatch creates a batch of Fabric's Orderer input envelopes.
func MapToEnvelopeBatch(_ uint64, txs []*protoloadgen.TX) []*common.Envelope {
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
func MapToOrdererBlock(blockNum uint64, txs []*protoloadgen.TX) *common.Block {
	data := make([][]byte, len(txs))
	for i, tx := range txs {
		data[i] = tx.SerializedEnvelope
	}
	return &common.Block{
		Header: &common.BlockHeader{Number: blockNum},
		Data:   &common.BlockData{Data: data},
	}
}

func makeRef(txID string, blockNum uint64, txNum int) *protocoordinatorservice.TxRef {
	return committerpb.TxRef(txID, blockNum, uint32(txNum)) //nolint:gosec // int -> uint32.
}
