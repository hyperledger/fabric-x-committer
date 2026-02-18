/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package sidecar

import (
	"io"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	ab "github.com/hyperledger/fabric-protos-go-apiv2/orderer"
	"github.com/hyperledger/fabric-protos-go-apiv2/peer"
	"github.com/hyperledger/fabric-x-common/common/ledger/blockledger"
	"github.com/hyperledger/fabric-x-common/common/util"
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-committer/utils/grpcerror"
	"github.com/hyperledger/fabric-x-committer/utils/serialization"
)

// blockDelivery implements peer.DeliverServer by streaming blocks from a blockStore.
type blockDelivery struct {
	blockStore *blockStore
	channelID  string
}

func newBlockDelivery(bs *blockStore, channelID string) *blockDelivery {
	return &blockDelivery{blockStore: bs, channelID: channelID}
}

// Deliver delivers the requested blocks.
func (s *blockDelivery) Deliver(srv peer.Deliver_DeliverServer) error {
	addr := util.ExtractRemoteAddress(srv.Context())
	logger.Infof("Starting new deliver loop for %s", addr)
	for {
		logger.Infof("Attempting to read seek info message from %s", addr)
		envelope, err := srv.Recv()
		if errors.Is(err, io.EOF) {
			logger.Infof("Received EOF from %s,", addr)
			return nil
		}
		if err != nil {
			return grpcerror.WrapInternalError(err)
		}

		logger.Infof("Received seek info message from %s", addr)
		status, err := s.deliverBlocks(srv, envelope)
		if err != nil {
			logger.Infof("Failed delivering to %s with status %v: %v", addr, status, err)
			return wrapDeliverError(status, err)
		}
		logger.Infof("Done delivering to %s", addr)

		if err = srv.Send(&peer.DeliverResponse{
			Type: &peer.DeliverResponse_Status{Status: status},
		}); err != nil {
			logger.Infof("Error sending to %s: %s", addr, err)
			return grpcerror.WrapInternalError(err)
		}
	}
}

// DeliverFiltered implements an API in peer.DeliverServer.
// Deprecated: this method is implemented to have compatibility with Fabric so that the fabric smart client
// can easily integrate with both FabricX and Fabric. Eventually, this method will be removed.
func (*blockDelivery) DeliverFiltered(peer.Deliver_DeliverFilteredServer) error {
	return grpcerror.WrapUnimplemented(errors.New("method is deprecated"))
}

// DeliverWithPrivateData implements an API in peer.DeliverServer.
// Deprecated: this method is implemented to have compatibility with Fabric so that the fabric smart client
// can easily integrate with both FabricX and Fabric. Eventually, this method will be removed.
func (*blockDelivery) DeliverWithPrivateData(peer.Deliver_DeliverWithPrivateDataServer) error {
	return grpcerror.WrapUnimplemented(errors.New("method is deprecated"))
}

func (s *blockDelivery) deliverBlocks(
	srv peer.Deliver_DeliverServer,
	envelope *common.Envelope,
) (common.Status, error) {
	payload, chdr, err := serialization.ParseEnvelope(envelope)
	if err != nil {
		return common.Status_BAD_REQUEST, errors.Wrap(err, "error parsing envelope")
	}

	if chdr.ChannelId != s.channelID {
		// Note, we log this at DEBUG because SDKs will poll waiting for channels to be created
		// So we would expect our log to be somewhat flooded with these
		return common.Status_NOT_FOUND, errors.New("channel not found")
	}

	cursor, stopNum, err := s.getCursor(payload.Data)
	if err != nil {
		return common.Status_BAD_REQUEST, err
	}
	logger.Debugf("Received seekInfo.")

	for srv.Context().Err() == nil {
		block, status := cursor.Next()

		if status != common.Status_SUCCESS {
			return status, nil
		}

		if err := srv.Send(&peer.DeliverResponse{Type: &peer.DeliverResponse_Block{Block: block}}); err != nil {
			return common.Status_INTERNAL_SERVER_ERROR, errors.Wrap(err, "error sending response")
		}
		logger.Infof("Successfully sent block %d:%d to client.", block.Header.Number, len(block.Data.Data))

		if stopNum == block.Header.Number {
			break
		}
	}
	return common.Status_SUCCESS, nil
}

func (s *blockDelivery) getCursor(payload []byte) (blockledger.Iterator, uint64, error) {
	seekInfo := &ab.SeekInfo{}
	if err := proto.Unmarshal(payload, seekInfo); err != nil {
		return nil, 0, errors.New("malformed seekInfo payload")
	}
	if seekInfo.Start == nil || seekInfo.Stop == nil {
		return nil, 0, errors.New("seekInfo missing start or stop")
	}

	cursor, number := s.blockStore.ledger.Iterator(seekInfo.Start)

	switch stop := seekInfo.Stop.Type.(type) {
	case *ab.SeekPosition_Oldest:
		return cursor, number, nil
	case *ab.SeekPosition_Newest:
		// when seeking only the newest block (i.e. starting
		// and stopping at newest), don't reevaluate the ledger
		// height as this can lead to multiple blocks being
		// sent when only one is expected
		if proto.Equal(seekInfo.Start, seekInfo.Stop) {
			return cursor, number, nil
		}
		return cursor, s.blockStore.ledger.Height() - 1, nil
	case *ab.SeekPosition_Specified:

		if stop.Specified.Number < number {
			return nil, 0, errors.New("start number greater than stop number")
		}
		return cursor, stop.Specified.Number, nil
	default:
		return nil, 0, errors.New("unknown type")
	}
}

// wrapDeliverError wraps deliver errors with appropriate gRPC status codes based on the Fabric status.
func wrapDeliverError(status common.Status, err error) error {
	if err == nil {
		return nil
	}
	switch status {
	case common.Status_BAD_REQUEST:
		return grpcerror.WrapInvalidArgument(err)
	case common.Status_NOT_FOUND:
		return grpcerror.WrapNotFound(err)
	default:
		return grpcerror.WrapInternalError(err)
	}
}
