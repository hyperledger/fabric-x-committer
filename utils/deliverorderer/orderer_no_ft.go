/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package deliverorderer

import (
	"context"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-protos-go-apiv2/orderer"
	"github.com/hyperledger/fabric-x-common/api/types"
	"github.com/hyperledger/fabric-x-common/common/channelconfig"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/deliver"
	"github.com/hyperledger/fabric-x-committer/utils/ordererdial"
)

// NoFTParameters needed for deliver to run.
type NoFTParameters struct {
	ClientConfig *ordererdial.Config
	NextBlockNum uint64
	OutputBlock  chan<- *common.Block
}

// ToQueueWithNoFT connects to an orderer delivery server and delivers the stream to a queue (go channel).
// It provides a simple, no verification method, to load blocks the an orderer.
// It should not be used in production.
// It returns when an error occurs or when the context is done.
// It will attempt to reconnect on errors.
func ToQueueWithNoFT(ctx context.Context, noFtParams NoFTParameters) error {
	params, err := LoadParametersFromConfig(noFtParams.ClientConfig)
	if err != nil {
		return err
	}
	configMaterial, err := channelconfig.LoadConfigBlockMaterial(params.LatestKnownConfig)
	if configMaterial == nil || err != nil {
		return err
	}

	m := ordererdial.NewDialInfo(configMaterial, ordererdial.Parameters{
		API:   types.Deliver,
		TLS:   params.TLS,
		Retry: params.Retry,
	})
	conn, connErr := m.Joint.NewLoadBalancedConnection()
	if connErr != nil {
		return connErr
	}
	defer connection.CloseConnectionsLog(conn)
	return deliver.ToQueue(ctx, deliver.Parameters{
		Deliverer:    &ordererDeliverer{client: orderer.NewAtomicBroadcastClient(conn)},
		ChannelID:    configMaterial.ChannelID,
		Signer:       params.Signer,
		TLSCertHash:  params.TLSCertHash,
		OutputBlock:  noFtParams.OutputBlock,
		NextBlockNum: noFtParams.NextBlockNum,
		HeaderOnly:   false,
	})
}

type ordererDeliverer struct {
	client orderer.AtomicBroadcastClient
}

func (d *ordererDeliverer) Deliver(ctx context.Context) (deliver.Streamer, error) {
	deliverStream, deliverErr := d.client.Deliver(ctx)
	if deliverErr != nil {
		return nil, deliverErr
	}
	return &ordererDeliverStream{AtomicBroadcast_DeliverClient: deliverStream}, nil
}

// ordererDeliverStream implements deliver.Streamer.
type ordererDeliverStream struct {
	orderer.AtomicBroadcast_DeliverClient
}

// RecvBlockOrStatus receives the created block from the ordering service. The first
// block number to be received is dependent on the seek position
// sent in DELIVER_SEEK_INFO message.
func (c *ordererDeliverStream) RecvBlockOrStatus() (*common.Block, *common.Status, error) {
	msg, err := c.Recv()
	if err != nil {
		return nil, nil, err
	}
	switch t := msg.Type.(type) {
	case *orderer.DeliverResponse_Status:
		return nil, &t.Status, nil
	case *orderer.DeliverResponse_Block:
		return t.Block, nil, nil
	default:
		return nil, nil, errors.New("unexpected message")
	}
}
