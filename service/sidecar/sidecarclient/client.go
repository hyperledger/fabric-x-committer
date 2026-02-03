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
	commontypes "github.com/hyperledger/fabric-x-common/api/types"
	"google.golang.org/grpc"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/deliver"
	"github.com/hyperledger/fabric-x-committer/utils/ordererconn"
)

type (
	// Client allow connecting to the sidecar to deliver blocks.
	Client struct {
		deliver.CftClient
	}

	// Parameters used to define the connection properties to the sidecar.
	Parameters struct {
		Client    *connection.ClientConfig
		ChannelID string
	}

	// DeliverParameters needed for deliver to run.
	// This is copy of deliver.Parameters to allow easy divergence in the future.
	DeliverParameters struct {
		StartBlkNum int64
		EndBlkNum   uint64
		OutputBlock chan<- *common.Block
	}

	// ledgerDeliverStream implements deliver.Stream.
	ledgerDeliverStream struct {
		peer.Deliver_DeliverClient
	}
)

// New instantiate a new sidecar client.
func New(config *Parameters) (*Client, error) {
	cm, err := ordererconn.NewConnectionManager(&ordererconn.Config{
		TLS:   ordererconn.TLSConfigToOrdererTLSConfig(config.Client.TLS),
		Retry: config.Client.Retry,
		Organizations: map[string]*ordererconn.OrganizationConfig{
			"org": {
				Endpoints: []*commontypes.OrdererEndpoint{{
					Host: config.Client.Endpoint.Host,
					Port: config.Client.Endpoint.Port,
				}},
				CACerts: config.Client.TLS.CACertPaths,
			},
		},
	})
	if err != nil {
		return nil, err
	}
	return &Client{
		CftClient: deliver.CftClient{
			ConnectionManager: cm,
			ChannelID:         config.ChannelID,
			StreamCreator: func(ctx context.Context, conn grpc.ClientConnInterface) (
				deliver.Stream, error,
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

// CloseConnections closes all the connections.
func (c *Client) CloseConnections() {
	c.ConnectionManager.CloseConnections()
}

// Deliver start receiving blocks starting from config.StartBlkNum to config.OutputBlock.
// The value of config.StartBlkNum is updated with the latest block number.
// This is a wrapper for CftClient.Deliver to allow easy divergence in the future.
func (c *Client) Deliver(ctx context.Context, config *DeliverParameters) error {
	return c.CftClient.Deliver(ctx, &deliver.Parameters{
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
