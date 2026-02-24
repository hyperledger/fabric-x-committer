/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package deliver

import (
	"context"
	"math"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-protos-go-apiv2/orderer"
	"github.com/hyperledger/fabric-x-common/common/util"
	"github.com/hyperledger/fabric-x-common/protoutil"
	"github.com/hyperledger/fabric-x-common/protoutil/identity"
	"google.golang.org/grpc"

	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/ordererconn"
)

type (
	// CftClient allows delivering blocks from one connection at a time.
	// If one connection fails, it will try to connect to another one.
	CftClient struct {
		ConnectionManager *ordererconn.ConnectionManager
		Signer            identity.SignerSerializer
		ChannelID         string
		StreamCreator     func(ctx context.Context, conn grpc.ClientConnInterface) (Stream, error)
	}

	// Parameters needed for deliver to run.
	Parameters struct {
		NextBlockNum uint64
		OutputBlock  chan<- *common.Block
	}

	// Stream requires the following interface.
	Stream interface {
		Send(*common.Envelope) error
		RecvBlockOrStatus() (*common.Block, *common.Status, error)
		Context() context.Context
	}
)

// MaxBlockNum is used for endless deliver.
const MaxBlockNum uint64 = math.MaxUint64

var defaultRetryProfile = connection.RetryProfile{}

// Deliver start receiving blocks starting from config.StartBlkNum to config.OutputBlock.
// The value of config.StartBlkNum is updated with the latest block number.
func (c *CftClient) Deliver(ctx context.Context, p *Parameters) error {
	for ctx.Err() == nil {
		err := c.receiveFromBlockDeliverer(ctx, p)
		logger.Warnf("Error receiving blocks: %v", err)
	}
	logger.Debugf("Deliver context ended: %v", ctx.Err())
	return errors.Wrap(ctx.Err(), "context ended")
}

func (c *CftClient) receiveFromBlockDeliverer(ctx context.Context, p *Parameters) error {
	logger.Debugf("Deliver is waiting for connection")
	conn, connErr := c.getConnection(ctx)
	if connErr != nil {
		return connErr
	}

	// We create a new context per stream to ensure it cancels on error.
	sCtx, sCancel := context.WithCancel(ctx)
	defer sCancel()
	logger.Debugf("Connecting to %s", conn.Target())
	stream, streamErr := c.StreamCreator(sCtx, conn)
	if streamErr != nil {
		return errors.Wrap(streamErr, "failed to create stream")
	}

	//nolint:contextcheck // false positive (stream's context is inherited from sCtx).
	addr := util.ExtractRemoteAddress(stream.Context())
	logger.Infof("Deliver connected to %s", addr)

	deliverRetry := defaultRetryProfile.NewBackoff()
	for sCtx.Err() == nil {
		status, err := c.deliverRelay(sCtx, stream, p)
		if err != nil {
			return err
		}
		if status == common.Status_SUCCESS {
			// Indication that the seek range is fully delivered.
			return nil
		}

		logger.Infof("Deliver failed with status: %s", status.String())
		// This is a workaround for the case when the start block is not yet available.
		backoffErr := connection.WaitForNextBackOffDuration(sCtx, deliverRetry)
		if errors.Is(backoffErr, connection.ErrRetryTimeout) {
			return backoffErr
		}
	}
	return nil
}

// getConnection returns a connection to a delivery service.
// We always ask the connection manager for the connection as this is not done often.
// If the endpoints haven't changed, the manager will return the exact same connection.
// If no connection available, we wait and try again.
func (c *CftClient) getConnection(ctx context.Context) (*grpc.ClientConn, error) {
	getConnRetry := defaultRetryProfile.NewBackoff()
	for ctx.Err() == nil {
		conn, _ := c.ConnectionManager.GetConnection(ordererconn.WithAPI(ordererconn.Deliver))
		if conn != nil {
			return conn, nil
		}
		logger.Infof("No available connection to deliver block")
		backoffErr := connection.WaitForNextBackOffDuration(ctx, getConnRetry)
		if errors.Is(backoffErr, connection.ErrRetryTimeout) {
			return nil, errors.Join(ordererconn.ErrNoConnections, backoffErr)
		}
	}
	return nil, errors.Join(ordererconn.ErrNoConnections, ctx.Err())
}

// deliverRelay initiate a new seek request and relays the delivered blocks to the output channel.
func (c *CftClient) deliverRelay(
	ctx context.Context, stream Stream, p *Parameters,
) (common.Status, error) {
	logger.Infof("Sending seek request from block %d on channel %s.", p.NextBlockNum, c.ChannelID)
	seekEnv, seekErr := seekSince(p.NextBlockNum, c.ChannelID, c.Signer)
	if seekErr != nil {
		return 0, errors.Wrap(seekErr, "failed to create seek request")
	}
	if err := stream.Send(seekEnv); err != nil {
		return 0, errors.Wrap(err, "failed to send seek request")
	}
	logger.Info("Seek request sent.")

	outputBlock := channel.NewWriter(ctx, p.OutputBlock)
	for ctx.Err() == nil {
		block, status, err := stream.RecvBlockOrStatus()
		if err != nil {
			return 0, errors.Wrap(err, "failed to receive block")
		}
		if status != nil {
			return *status, nil
		}

		// We make minimal verifications to ensure we receive blocks in order.
		// This allows us to restart the connection from the next expected block upon failure.
		if block == nil || block.Header == nil {
			return 0, errors.New("received nil block or with nil header")
		}
		if block.Header.Number != p.NextBlockNum {
			return 0, errors.Errorf("received block number %d != %d (expected)",
				block.Header.Number, p.NextBlockNum)
		}

		p.NextBlockNum = block.Header.Number + 1
		logger.Debugf("next expected block number is %d", p.NextBlockNum)
		outputBlock.Write(block)
	}
	return 0, errors.Wrap(ctx.Err(), "context ended")
}

// TODO: We have seek info only for the orderer but not for the ledger service. It needs
//       to implemented as fabric ledger also allows different seek info.

func seekSince(
	startBlockNumber uint64,
	channelID string,
	signer identity.SignerSerializer,
) (*common.Envelope, error) {
	return protoutil.CreateSignedEnvelope(
		common.HeaderType_DELIVER_SEEK_INFO, channelID, signer, &orderer.SeekInfo{
			Start:    seekPosition(startBlockNumber),
			Stop:     seekPosition(MaxBlockNum),
			Behavior: orderer.SeekInfo_BLOCK_UNTIL_READY,
		}, 0, 0,
	)
}

func seekPosition(blockNumber uint64) *orderer.SeekPosition {
	return &orderer.SeekPosition{Type: &orderer.SeekPosition_Specified{Specified: &orderer.SeekSpecified{
		Number: blockNumber,
	}}}
}
