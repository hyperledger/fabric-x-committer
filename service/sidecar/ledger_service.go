/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package sidecar

import (
	"context"
	"io"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-lib-go/common/metrics/disabled"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	ab "github.com/hyperledger/fabric-protos-go-apiv2/orderer"
	"github.com/hyperledger/fabric-protos-go-apiv2/peer"
	"github.com/hyperledger/fabric-x-common/common/ledger/blockledger"
	"github.com/hyperledger/fabric-x-common/common/ledger/blockledger/fileledger"
	"github.com/hyperledger/fabric-x-common/common/util"
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/grpcerror"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring/promutil"
	"github.com/hyperledger/fabric-x-committer/utils/serialization"
)

type (
	// ledgerService implements peer.DeliverServer.
	ledgerService struct {
		ledger                       blockledger.ReadWriter
		ledgerProvider               blockledger.Factory
		channelID                    string
		nextToBeCommittedBlockNumber uint64
		metrics                      *perfMetrics
	}

	// ledgerRunConfig holds the configuration needed to run the ledger service.
	ledgerRunConfig struct {
		IncomingCommittedBlock <-chan *common.Block
	}
)

// newLedgerService creates a new ledger service.
func newLedgerService(channelID, ledgerDir string, metrics *perfMetrics) (*ledgerService, error) {
	logger.Infof("Create ledger files for channel %s under %s", channelID, ledgerDir)
	factory, err := fileledger.New(ledgerDir, &disabled.Provider{})
	if err != nil {
		return nil, err
	}

	ledger, err := factory.GetOrCreate(channelID)
	if err != nil {
		return nil, err
	}

	return &ledgerService{
		ledger:                       ledger,
		ledgerProvider:               factory,
		channelID:                    channelID,
		nextToBeCommittedBlockNumber: ledger.Height(),
		metrics:                      metrics,
	}, nil
}

// run starts the ledger service. The call to run blocks until an error occurs or the context is canceled.
func (s *ledgerService) run(ctx context.Context, config *ledgerRunConfig) error {
	inputBlock := channel.NewReader(ctx, config.IncomingCommittedBlock)
	for {
		block, ok := inputBlock.Read()
		if !ok {
			return nil
		}

		if block.Header.Number < s.nextToBeCommittedBlockNumber {
			// NOTE: The block store height can be greater than the state database
			//       height. This is because the last committed block number is
			//       updated in the state database periodically, while blocks are
			//       written to the block store immediately. Consequently, it's
			//       possible to receive a block that is already present in the
			//       block store when the sidecar recovers after a failure, or when the
			//       coordinator recovers after a failure.
			continue
		} else if block.Header.Number > s.nextToBeCommittedBlockNumber {
			return errors.Newf("block store expects block number [%d] but received a greater block number [%d]",
				s.nextToBeCommittedBlockNumber, block.Header.Number)
		}
		s.nextToBeCommittedBlockNumber++

		logger.Debugf("Appending block %d to ledger.", block.Header.Number)
		start := time.Now()
		if err := s.ledger.Append(block); err != nil {
			return err
		}
		promutil.Observe(s.metrics.appendBlockToLedgerSeconds, time.Since(start))
		promutil.SetUint64Gauge(s.metrics.blockHeight, block.Header.Number+1)
		logger.Debugf("Appended block %d to ledger.", block.Header.Number)
	}
}

// close releases the ledger directory.
func (s *ledgerService) close() {
	s.ledgerProvider.Close()
}

// Deliver delivers the requested blocks.
func (s *ledgerService) Deliver(srv peer.Deliver_DeliverServer) error {
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
func (*ledgerService) DeliverFiltered(peer.Deliver_DeliverFilteredServer) error {
	return grpcerror.WrapUnimplemented(errors.New("method is deprecated"))
}

// DeliverWithPrivateData implements an API in peer.DeliverServer.
// Deprecated: this method is implemented to have compatibility with Fabric so that the fabric smart client
// can easily integrate with both FabricX and Fabric. Eventually, this method will be removed.
func (*ledgerService) DeliverWithPrivateData(peer.Deliver_DeliverWithPrivateDataServer) error {
	return grpcerror.WrapUnimplemented(errors.New("method is deprecated"))
}

// GetBlockHeight returns the height of the block store, i.e., the last committed block + 1. The +1 is needed
// to include block 0 as well.
func (s *ledgerService) GetBlockHeight() uint64 {
	return s.ledger.Height()
}

func (s *ledgerService) deliverBlocks(
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

func (s *ledgerService) getCursor(payload []byte) (blockledger.Iterator, uint64, error) {
	seekInfo := &ab.SeekInfo{}
	if err := proto.Unmarshal(payload, seekInfo); err != nil {
		return nil, 0, errors.New("malformed seekInfo payload")
	}
	if seekInfo.Start == nil || seekInfo.Stop == nil {
		return nil, 0, errors.New("seekInfo missing start or stop")
	}

	cursor, number := s.ledger.Iterator(seekInfo.Start)

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
		return cursor, s.ledger.Height() - 1, nil
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
