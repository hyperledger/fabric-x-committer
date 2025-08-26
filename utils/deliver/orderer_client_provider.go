/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package deliver

import (
	"errors"

	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-protos-go-apiv2/orderer"
)

// ordererDeliverStream implements Stream.
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
