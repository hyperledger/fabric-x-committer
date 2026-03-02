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

// LoadConfigBlockFromFile loads a config block from a file.
// If the block is not a config block, nil will be returned without an error.
func LoadConfigBlockFromFile(blockPath string) (*ConfigBlockMaterial, error) {
	configBlock, err := configtxgen.ReadBlock(blockPath)
	if err != nil {
		return nil, err
	}
	return LoadConfigBlock(configBlock)
}

// LoadConfigBlock attempts to read a config block from the given block.
// If the block is not a config block, nil will be returned without an error.
func LoadConfigBlock(block *common.Block) (*ConfigBlockMaterial, error) {
	if block == nil || block.Data == nil {
		return nil, nil
	}
	// We expect config blocks to have exactly one transaction, with a valid payload.
	if len(block.Data.Data) != 1 {
		return nil, nil
	}
	configTx, err := protoutil.GetEnvelopeFromBlock(block.Data.Data[0])
	if err != nil {
		return nil, nil //nolint:nilerr // We don't care if the block content is not a config block.
	}
	payload, err := protoutil.UnmarshalPayload(configTx.Payload)
	if err != nil {
		return nil, nil //nolint:nilerr // We don't care if the block content is not a config block.
	}
	if payload.Header == nil {
		return nil, nil
	}
	chHead, err := protoutil.UnmarshalChannelHeader(payload.Header.ChannelHeader)
	if err != nil {
		return nil, nil //nolint:nilerr // We don't care if the block content is not a config block.
	}
	if chHead.Type != int32(common.HeaderType_CONFIG) {
		return nil, nil
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
