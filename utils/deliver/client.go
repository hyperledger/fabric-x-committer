/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package deliver

import (
	"context"

	"github.com/cockroachdb/errors"
	ab "github.com/hyperledger/fabric-protos-go-apiv2/orderer"
	"github.com/hyperledger/fabric-x-common/protoutil"
	"google.golang.org/grpc"

	"github.com/hyperledger/fabric-x-committer/utils/logging"
	"github.com/hyperledger/fabric-x-committer/utils/ordererconn"
)

type (
	// Client is a collection of nodes and their connections.
	Client struct {
		config            *ordererconn.Config
		connectionManager *ordererconn.ConnectionManager
		signer            protoutil.Signer
	}
)

var logger = logging.New("broadcast-deliver")

// New creates a broadcast/deliver client. It must be closed to release all the associated connections.
func New(config *ordererconn.Config) (*Client, error) {
	if err := ordererconn.ValidateConfig(config); err != nil {
		return nil, errors.Wrap(err, "error validating config")
	}

	signer, err := ordererconn.NewIdentitySigner(config.Identity)
	if err != nil {
		return nil, errors.Wrap(err, "error creating identity signer")
	}

	cm := &ordererconn.ConnectionManager{}
	if err = cm.Update(&config.Connection); err != nil {
		return nil, errors.Wrap(err, "error creating connections")
	}

	return &Client{
		config:            config,
		connectionManager: cm,
		signer:            signer,
	}, nil
}

// CloseConnections closes all the connections for the client.
func (s *Client) CloseConnections() {
	s.connectionManager.CloseConnections()
}

// UpdateConnections updates the connection config.
func (s *Client) UpdateConnections(config *ordererconn.ConnectionConfig) error {
	return s.connectionManager.Update(config)
}

// Deliver starts the block receiver. The call to Deliver blocks until an error occurs or the context is canceled.
func (s *Client) Deliver(ctx context.Context, p *Parameters) error {
	switch s.config.ConsensusType {
	case ordererconn.Bft:
		// TODO: We should borrow Fabric's delivery client implementation that supports BFT.
		logger.Warnf("deliver consensus type %s is not supported; falling back to %s",
			s.config.ConsensusType, ordererconn.Cft)
		fallthrough
	case ordererconn.Cft:
		c := &CftClient{
			ConnectionManager: s.connectionManager,
			Signer:            s.signer,
			ChannelID:         s.config.ChannelID,
			StreamCreator: func(ctx context.Context, conn grpc.ClientConnInterface) (Stream, error) {
				client, err := ab.NewAtomicBroadcastClient(conn).Deliver(ctx)
				if err != nil {
					return nil, err
				}
				return &ordererDeliverStream{client}, nil
			},
		}
		return c.Deliver(ctx, p)
	default:
		return errors.Newf("invalid consensus type: %s", s.config.ConsensusType)
	}
}
