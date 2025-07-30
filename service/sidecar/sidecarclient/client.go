/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package sidecarclient

import (
	"context"
	"errors"

	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-protos-go-apiv2/peer"
	"google.golang.org/grpc"

	"github.com/hyperledger/fabric-x-committer/utils/broadcastdeliver"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

type (
	// Client allow connecting to the sidecar to deliver blocks.
	Client struct {
		broadcastdeliver.DeliverCftClient
	}

	// Config is used to define the connection properties to the sidecar.
	Config struct {
		ChannelID string
		Client    *connection.ClientConfig
	}

	// DeliverConfig holds the configuration needed for deliver to run.
	// This is copy of broadcastdeliver.DeliverConfig to allow easy divergence in the future.
	DeliverConfig struct {
		StartBlkNum int64
		EndBlkNum   uint64
		OutputBlock chan<- *common.Block
	}

	// ledgerDeliverStream implements DeliverStream.
	ledgerDeliverStream struct {
		peer.Deliver_DeliverClient
	}
)

// New instantiate a new sidecar client.
func New(config *Config) (*Client, error) {
	cm := &broadcastdeliver.OrdererConnectionManager{}
	connConfig := &broadcastdeliver.ConnectionConfig{
		Endpoints: connection.ToOrdererEndpoints(config.Client.Endpoints...),
		Retry:     config.Client.Retry,
		Creds:     config.Client.Creds,
	}
	if err := cm.Update(connConfig); err != nil {
		return nil, err
	}
	return &Client{
		DeliverCftClient: broadcastdeliver.DeliverCftClient{
			ConnectionManager: cm,
			ChannelID:         config.ChannelID,
			StreamCreator: func(ctx context.Context, conn grpc.ClientConnInterface) (
				broadcastdeliver.DeliverStream, error,
			) {
				client, err := peer.NewDeliverClient(conn).Deliver(ctx)
				if err != nil {
					return nil, err
				}
				return &ledgerDeliverStream{client}, nil
			},
		},
	}, nil
}

// Deliver start receiving blocks starting from config.StartBlkNum to config.OutputBlock.
// The value of config.StartBlkNum is updated with the latest block number.
// This is a wrapper for DeliverCftClient.Deliver to allow easy divergence in the future.
func (c *Client) Deliver(ctx context.Context, config *DeliverConfig) error {
	return c.DeliverCftClient.Deliver(ctx, &broadcastdeliver.DeliverConfig{
		StartBlkNum: config.StartBlkNum,
		EndBlkNum:   config.EndBlkNum,
		OutputBlock: config.OutputBlock,
	})
}

// RecvBlockOrStatus receives the committed block from the ledger service. The first
// block number to be received is dependent on the seek position
// sent in DELIVER_SEEK_INFO message.
func (s *ledgerDeliverStream) RecvBlockOrStatus() (*common.Block, *common.Status, error) {
	msg, err := s.Recv()
	if err != nil {
		return nil, nil, err
	}
	switch t := msg.Type.(type) {
	case *peer.DeliverResponse_Status:
		return nil, &t.Status, nil
	case *peer.DeliverResponse_Block:
		return t.Block, nil, nil
	default:
		return nil, nil, errors.New("unexpected message")
	}
}
