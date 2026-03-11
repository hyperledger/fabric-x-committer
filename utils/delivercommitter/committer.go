/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package delivercommitter

import (
	"context"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-protos-go-apiv2/peer"
	commontypes "github.com/hyperledger/fabric-x-common/api/types"
	"google.golang.org/grpc"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/deliver"
	"github.com/hyperledger/fabric-x-committer/utils/ordererconn"
)

// Parameters needed for deliver to run.
type Parameters struct {
	ClientConfig *connection.ClientConfig
	NextBlockNum uint64
	OutputBlock  chan<- *common.Block
}

// ToQueue start receiving blocks starting from NextBlockNum to outputBlock.
func ToQueue(ctx context.Context, cdp Parameters) error {
	cm, err := ordererconn.NewConnectionManager(&ordererconn.Config{
		TLS:   ordererconn.TLSConfigToOrdererTLSConfig(cdp.ClientConfig.TLS),
		Retry: cdp.ClientConfig.Retry,
		Organizations: map[string]*ordererconn.OrganizationConfig{
			"org": {
				Endpoints: []*commontypes.OrdererEndpoint{{
					Host: cdp.ClientConfig.Endpoint.Host,
					Port: cdp.ClientConfig.Endpoint.Port,
				}},
				CACerts: cdp.ClientConfig.TLS.CACertPaths,
			},
		},
	})
	if err != nil {
		return err
	}
	defer cm.CloseConnections()
	c := deliver.CftClient{
		ConnectionManager: cm,
		StreamCreator: func(ctx context.Context, conn grpc.ClientConnInterface) (
			deliver.Stream, error,
		) {
			client, err := peer.NewDeliverClient(conn).Deliver(ctx)
			if err != nil {
				return nil, err
			}
			return &ledgerDeliverStream{client}, nil
		},
	}
	return c.Deliver(ctx, &deliver.Parameters{
		NextBlockNum: cdp.NextBlockNum,
		OutputBlock:  cdp.OutputBlock,
	})
}

// ledgerDeliverStream implements deliver.streamer.
type ledgerDeliverStream struct {
	peer.Deliver_DeliverClient
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
