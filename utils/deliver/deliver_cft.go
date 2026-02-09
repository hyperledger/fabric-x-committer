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
		StartBlkNum int64
		EndBlkNum   uint64
		OutputBlock chan<- *common.Block
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
		if p.StartBlkNum > 0 && uint64(p.StartBlkNum) > p.EndBlkNum {
			logger.Infof("Deliver finished successfully")
			return nil
		}
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
	logger.Infof("Sending seek request from block %d on channel %s.", p.StartBlkNum, c.ChannelID)
	seekEnv, seekErr := seekSince(p.StartBlkNum, p.EndBlkNum, c.ChannelID, c.Signer)
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

		//nolint:gosec // integer overflow conversion uint64 -> int64
		p.StartBlkNum = int64(block.Header.Number) + 1
		logger.Debugf("next expected block number is %d", p.StartBlkNum)
		outputBlock.Write(block)
	}
	return 0, errors.Wrap(ctx.Err(), "context ended")
}

// TODO: We have seek info only for the orderer but not for the ledger service. It needs
//       to implemented as fabric ledger also allows different seek info.

const (
	seekSinceOldestBlock = -2
	seekSinceNewestBlock = -1
)

var (
	oldest = &orderer.SeekPosition{Type: &orderer.SeekPosition_Oldest{Oldest: &orderer.SeekOldest{}}}
	newest = &orderer.SeekPosition{Type: &orderer.SeekPosition_Newest{Newest: &orderer.SeekNewest{}}}
)

func seekSince(
	startBlockNumber int64,
	endBlkNum uint64,
	channelID string,
	signer identity.SignerSerializer,
) (*common.Envelope, error) {
	var startPosition *orderer.SeekPosition
	switch startBlockNumber {
	case seekSinceOldestBlock:
		startPosition = oldest
	case seekSinceNewestBlock:
		startPosition = newest
	default:
		if startBlockNumber < -2 {
			return nil, errors.New("wrong seek value")
		}
		startPosition = &orderer.SeekPosition{Type: &orderer.SeekPosition_Specified{Specified: &orderer.SeekSpecified{
			Number: uint64(startBlockNumber), //nolint:gosec // integer overflow conversion int64 -> uint64
		}}}
	}

	return protoutil.CreateSignedEnvelope(
		common.HeaderType_DELIVER_SEEK_INFO, channelID, signer, &orderer.SeekInfo{
			Start: startPosition,
			Stop: &orderer.SeekPosition{Type: &orderer.SeekPosition_Specified{Specified: &orderer.SeekSpecified{
				Number: endBlkNum,
			}}},
			Behavior: orderer.SeekInfo_BLOCK_UNTIL_READY,
		}, 0, 0,
	)
}
