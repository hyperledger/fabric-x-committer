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

	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/deliver"
)

// Parameters needed for deliver to run.
type Parameters struct {
	ClientConfig *connection.ClientConfig
	NextBlockNum uint64
	OutputBlock  chan<- *common.Block
}

// ToQueue connects to a committer delivery server and delivers the stream to a queue (go channel).
// It returns when an error occurs or when the context is done.
// It will attempt to reconnect on errors.
func ToQueue(ctx context.Context, cdp Parameters) error {
	conn, err := connection.NewSingleConnection(cdp.ClientConfig)
	if err != nil {
		return err
	}
	defer connection.CloseConnectionsLog(conn)
	client := peer.NewDeliverClient(conn)
	return deliver.ToQueue(ctx, deliver.Parameters{
		NextBlockNum: cdp.NextBlockNum,
		OutputBlock:  cdp.OutputBlock,
		StreamCreator: func(ctx context.Context) (deliver.Streamer, error) {
			deliverStream, deliverErr := client.Deliver(ctx)
			if deliverErr != nil {
				return nil, deliverErr
			}
			return &ledgerDeliverStream{Deliver_DeliverClient: deliverStream}, nil
		},
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
