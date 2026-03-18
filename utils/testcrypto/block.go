/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package testcrypto

import (
	"slices"

	"github.com/hyperledger/fabric-lib-go/common/flogging"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-x-common/msp"
	"github.com/hyperledger/fabric-x-common/protoutil"
	"google.golang.org/protobuf/proto"
)

var logger = flogging.MustGetLogger("testcrypto")

// BlockPrepareParameters describe the parameters needed to prepare a valid block.
// Each field is optional, however missing fields may create an invalid block,
// depending on the verification level.
type BlockPrepareParameters struct {
	PrevBlock            *common.Block
	LastConfigBlockIndex uint64
	ConsenterMetadata    []byte
	ConsenterSigners     []msp.SigningIdentity
}

// PrepareBlockHeaderAndMetadata adds a valid header and metadata to the block.
func PrepareBlockHeaderAndMetadata(block *common.Block, p BlockPrepareParameters) *common.Block {
	block = proto.CloneOf(block)
	var blockNumber uint64
	var previousHash []byte
	if p.PrevBlock != nil {
		blockNumber = p.PrevBlock.Header.Number + 1
		previousHash = protoutil.BlockHeaderHash(p.PrevBlock.Header)
	}
	if block.Data == nil {
		block.Data = &common.BlockData{}
	}
	block.Header = &common.BlockHeader{
		Number:       blockNumber,
		DataHash:     protoutil.ComputeBlockDataHash(block.Data),
		PreviousHash: previousHash,
	}
	meta := block.Metadata
	if meta == nil {
		meta = &common.BlockMetadata{}
		block.Metadata = meta
	}
	expectedSize := len(common.BlockMetadataIndex_name)
	meta.Metadata = slices.Grow(meta.Metadata, max(0, expectedSize-cap(meta.Metadata)))[:expectedSize]

	// 1. Prepare the OrdererBlockMetadata payload
	lastConfigIdx := p.LastConfigBlockIndex
	if protoutil.IsConfigBlock(block) {
		// A config block points to itself.
		lastConfigIdx = blockNumber
	}
	ordererMetadata := &common.OrdererBlockMetadata{
		LastConfig:        &common.LastConfig{Index: lastConfigIdx},
		ConsenterMetadata: p.ConsenterMetadata,
	}
	ordererMetadataBytes := protoutil.MarshalOrPanic(ordererMetadata)
	blockHeaderBytes := protoutil.BlockHeaderBytes(block.Header)

	// 2. The value to be signed includes the OrdererBlockMetadata bytes and the Block Header
	// Fabric 3.0 signs the concatenation of (MetadataValue + BlockHeader)
	// Note: The Metadata.Value itself is the marshaled OrdererBlockMetadata
	sigs := make([]*common.MetadataSignature, len(p.ConsenterSigners))
	for i, signer := range p.ConsenterSigners {
		creator, err := signer.Serialize()
		if err != nil {
			logger.Warnf("failed to serialize signer: %v", err)
			continue
		}

		// The payload to sign is typically the OrdererBlockMetadata + BlockHeaderBytes
		// specifically for BFT/v3.0 consensus validation.
		signatureHeaderBytes := protoutil.MarshalOrPanic(&common.SignatureHeader{Creator: creator})

		// Concat: MetadataValue + SignatureHeader + BlockHeader
		signingPayload := slices.Concat(ordererMetadataBytes, signatureHeaderBytes, blockHeaderBytes)
		signature, err := signer.Sign(signingPayload)
		if err != nil {
			logger.Warnf("failed to sign orderer: %v", err)
			continue
		}

		sigs[i] = &common.MetadataSignature{
			SignatureHeader: signatureHeaderBytes,
			Signature:       signature,
		}
	}

	// 3. Assemble the final Metadata structure at the SIGNATURES index
	meta.Metadata[common.BlockMetadataIndex_SIGNATURES] = protoutil.MarshalOrPanic(&common.Metadata{
		Value:      ordererMetadataBytes,
		Signatures: sigs,
	})
	return block
}
