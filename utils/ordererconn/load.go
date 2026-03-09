/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package ordererconn

import (
	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-lib-go/bccsp/factory"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-x-common/common/channelconfig"
	"github.com/hyperledger/fabric-x-common/common/configtx"
	"github.com/hyperledger/fabric-x-common/protoutil"
	"github.com/hyperledger/fabric-x-common/tools/configtxgen"
)

type (
	// ConfigBlockMaterial contains the channel-ID, the config block, and its bundle.
	ConfigBlockMaterial struct {
		ChannelID   string
		ConfigBlock *common.Block
		Bundle      *channelconfig.Bundle
	}
)

// ErrNotConfigBlock is returned when the block is not a config block.
var ErrNotConfigBlock = errors.New("the block is not a config block")

// LoadConfigBlockFromFile loads a config block from a file.
// If the block is not a config block, ErrNotConfigBlock will be returned.
func LoadConfigBlockFromFile(blockPath string) (*ConfigBlockMaterial, error) {
	configBlock, err := configtxgen.ReadBlock(blockPath)
	if err != nil {
		return nil, err
	}
	return LoadConfigBlock(configBlock)
}

// LoadConfigBlock attempts to read a config block from the given block.
// If the block is not a config block, ErrNotConfigBlock will be returned.
func LoadConfigBlock(block *common.Block) (*ConfigBlockMaterial, error) {
	// We expect config blocks to have exactly one transaction, with a valid payload.
	if block == nil || block.Data == nil || len(block.Data.Data) != 1 {
		return nil, ErrNotConfigBlock
	}
	configTx, err := protoutil.GetEnvelopeFromBlock(block.Data.Data[0])
	if err != nil {
		return nil, errors.Join(ErrNotConfigBlock, err)
	}

	payload, err := protoutil.UnmarshalPayload(configTx.Payload)
	if err != nil {
		return nil, errors.Join(ErrNotConfigBlock, err)
	}
	if payload.Header == nil {
		return nil, ErrNotConfigBlock
	}
	chHead, err := protoutil.UnmarshalChannelHeader(payload.Header.ChannelHeader)
	if err != nil || chHead.Type != int32(common.HeaderType_CONFIG) {
		return nil, errors.Join(ErrNotConfigBlock, err)
	}

	// This is a config block. Let's parse it.
	configEnvelope, err := configtx.UnmarshalConfigEnvelope(payload.Data)
	if err != nil {
		return nil, errors.Wrap(err, "error unmarshalling config envelope from payload data")
	}

	bundle, err := channelconfig.NewBundle(chHead.ChannelId, configEnvelope.Config, factory.GetDefault())
	if err != nil {
		return nil, errors.Wrap(err, "error creating channel config bundle")
	}
	return &ConfigBlockMaterial{
		ChannelID:   chHead.ChannelId,
		ConfigBlock: block,
		Bundle:      bundle,
	}, nil
}
