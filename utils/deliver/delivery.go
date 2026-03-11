/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package deliver

import (
	"context"
	"math"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-lib-go/common/flogging"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-protos-go-apiv2/orderer"
	"github.com/hyperledger/fabric-x-common/common/util"
	"github.com/hyperledger/fabric-x-common/protoutil"
	"github.com/hyperledger/fabric-x-common/protoutil/identity"

	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

type (
	// Parameters holds the parameters for delivery.
	Parameters struct {
		StreamCreator           func(ctx context.Context) (Streamer, error)
		ChannelID               string
		Signer                  identity.SignerSerializer
		TLSCertHash             []byte
		NextBlockNum            uint64
		OutputBlock             chan<- *common.Block
		OutputBlockWithSourceID chan<- *BlockWithSourceID
		HeaderOnly              bool
		SourceID                uint32
		Retry                   *connection.RetryProfile
	}

	// BlockWithSourceID can provide extra information on the block source when aggregating multiple
	// sources into a single output channel.
	BlockWithSourceID struct {
		SourceID uint32
		Block    *common.Block
	}

	// Streamer requires the following interface.
	Streamer interface {
		Context() context.Context
		Send(*common.Envelope) error
		RecvBlockOrStatus() (*common.Block, *common.Status, error)
	}
)

var logger = flogging.MustGetLogger("deliver")

// ToQueue connects to a delivery server and delivers the stream to a queue (go channel).
// It returns when an error occurs or when the context is done.
// It will attempt to reconnect on errors.
func ToQueue(ctx context.Context, p Parameters) error {
	return connection.Sustain(ctx, p.Retry, func() error {
		return toQueueWithoutReconnect(ctx, &p)
	})
}

// toQueueWithoutReconnect connects to a delivery server and delivers the stream for
// the specified channel-id to a queue (go channel).
// The value of p.NextBlockNum is updated with the latest block number.
// It returns when an error occurs or when the context is done.
// It will NOT attempt to reconnect on errors.
func toQueueWithoutReconnect(ctx context.Context, p *Parameters) error {
	seekEnv, seekErr := newSeekRequest(p)
	if seekErr != nil {
		return errors.Join(connection.ErrNonRetryable, errors.Wrap(seekErr, "failed to create seek request"))
	}

	// We create a new context per stream to ensure it cancels on error.
	cCtx, sCancel := context.WithCancel(ctx)
	defer sCancel()
	stream, streamErr := p.StreamCreator(cCtx)
	if streamErr != nil {
		return errors.Join(connection.ErrBackOff, errors.Wrap(streamErr, "failed to create stream"))
	}
	sCtx := stream.Context()

	//nolint:contextcheck // false positive (stream's context is inherited from cCtx).
	addr := util.ExtractRemoteAddress(sCtx)
	logger.Infof("Deliver connected to %s", addr)

	logger.Infof("Sending seek request from block %d on channel %s.", p.NextBlockNum, p.ChannelID)
	if err := stream.Send(seekEnv); err != nil {
		return errors.Join(connection.ErrBackOff, errors.Wrap(err, "failed to send seek request"))
	}

	//nolint:contextcheck // false positive (stream's context is inherited from cCtx).
	outputBlock := channel.NewWriter(sCtx, p.OutputBlock)
	//nolint:contextcheck // false positive (stream's context is inherited from cCtx).
	outputBlockWithSourceID := channel.NewWriter(sCtx, p.OutputBlockWithSourceID)

	// Initially, backoff on error. But upon receiving fhe first block, we reset the backoff.
	backoff := connection.ErrBackOff
	for sCtx.Err() == nil {
		logger.Debugf("Next expected block number is %d", p.NextBlockNum)
		block, status, err := stream.RecvBlockOrStatus()
		if err != nil {
			return errors.Join(backoff, errors.Wrap(err, "failed to receive block"))
		}
		if status != nil {
			return errors.Join(backoff, errors.Newf("delivery status: %s", status.String()))
		}

		// We make minimal verifications to ensure we receive blocks in order.
		// This allows us to restart the connection from the next expected block.
		// We restart the connection upon failure.
		if block == nil || block.Header == nil {
			return errors.Join(backoff, errors.New("received nil block or with nil header"))
		}
		if block.Header.Number != p.NextBlockNum {
			return errors.Join(backoff, errors.Errorf("received block number %d != %d (expected)",
				block.Header.Number, p.NextBlockNum))
		}
		p.NextBlockNum++
		backoff = nil

		// Write will be no-op if the output buffer is nil.
		outputBlock.Write(block)
		outputBlockWithSourceID.Write(&BlockWithSourceID{
			SourceID: p.SourceID,
			Block:    block,
		})
	}
	return errors.Wrap(sCtx.Err(), "context ended")
}

// TODO: We have seek info only for the orderer but not for the ledger service. It needs
//       to implemented as fabric ledger also allows different seek info.

func newSeekRequest(p *Parameters) (*common.Envelope, error) {
	contentType := orderer.SeekInfo_BLOCK
	if p.HeaderOnly {
		contentType = orderer.SeekInfo_HEADER_WITH_SIG
	}
	return protoutil.CreateSignedEnvelopeWithTLSBinding(
		common.HeaderType_DELIVER_SEEK_INFO, p.ChannelID, p.Signer, &orderer.SeekInfo{
			Start:       newSeekPosition(p.NextBlockNum),
			Stop:        newSeekPosition(math.MaxUint64),
			Behavior:    orderer.SeekInfo_BLOCK_UNTIL_READY,
			ContentType: contentType,
		}, 0, 0, p.TLSCertHash,
	)
}

func newSeekPosition(nextBlockNum uint64) *orderer.SeekPosition {
	return &orderer.SeekPosition{Type: &orderer.SeekPosition_Specified{Specified: &orderer.SeekSpecified{
		Number: nextBlockNum,
	}}}
}
