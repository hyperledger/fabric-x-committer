/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package adapters

import (
	"context"
	"fmt"
	"strings"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"golang.org/x/sync/errgroup"

	"github.com/hyperledger/fabric-x-committer/loadgen/metrics"
	"github.com/hyperledger/fabric-x-committer/service/sidecar/sidecarclient"
	"github.com/hyperledger/fabric-x-committer/utils"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/deliver"
	"github.com/hyperledger/fabric-x-committer/utils/serialization"
)

type sidecarReceiverParameters struct {
	Res          *ClientResources
	ClientConfig *connection.ClientConfig
}

const (
	committedBlocksQueueSize = 1024
	statusIdx                = int(common.BlockMetadataIndex_TRANSACTIONS_FILTER)
)

// runSidecarReceiver start receiving blocks from the sidecar.
func runSidecarReceiver(ctx context.Context, params *sidecarReceiverParameters) error {
	ledgerReceiver, err := sidecarclient.New(&sidecarclient.Parameters{
		ChannelID: params.Res.Profile.Policy.ChannelID,
		Client:    params.ClientConfig,
	})
	if err != nil {
		return err
	}
	return runDeliveryReceiver(ctx, params.Res, func(gCtx context.Context, committedBlock chan *common.Block) error {
		return ledgerReceiver.Deliver(gCtx, &sidecarclient.DeliverParameters{
			OutputBlock: committedBlock,
		})
	})
}

// runOrdererReceiver start receiving blocks from the orderer.
func runOrdererReceiver(ctx context.Context, res *ClientResources, client *deliver.Client) error {
	return runDeliveryReceiver(ctx, res, func(gCtx context.Context, committedBlock chan *common.Block) error {
		return client.Deliver(gCtx, &deliver.Parameters{
			OutputBlock: committedBlock,
		})
	})
}

// runDeliveryReceiver start receiving blocks from a delivery service.
func runDeliveryReceiver(
	ctx context.Context, res *ClientResources, deliverMethod func(context.Context, chan *common.Block) error,
) error {
	g, gCtx := errgroup.WithContext(ctx)
	committedBlock := make(chan *common.Block, committedBlocksQueueSize)
	g.Go(func() error {
		return deliverMethod(gCtx, committedBlock)
	})
	g.Go(func() error {
		receiveCommittedBlock(gCtx, committedBlock, res)
		return context.Canceled
	})
	return errors.Wrap(g.Wait(), "receiver done")
}

func receiveCommittedBlock(
	ctx context.Context,
	blockQueue <-chan *common.Block,
	res *ClientResources,
) {
	pCtx, pCancel := context.WithCancel(ctx)
	defer pCancel()
	committedBlock := channel.NewReader(pCtx, blockQueue)
	processedBlocks := channel.Make[[]metrics.TxStatus](pCtx, cap(blockQueue))

	// Pipeline the de-serialization process.
	go func() {
		for pCtx.Err() == nil {
			block, ok := committedBlock.Read()
			if !ok {
				return
			}
			processedBlocks.Write(mapToStatusBatch(block))
		}
	}()

	for pCtx.Err() == nil {
		statusBatch, ok := processedBlocks.Read()
		if !ok {
			return
		}
		res.Metrics.OnReceiveBatch(statusBatch)
		if res.isReceiveLimit() {
			return
		}
	}
}

// mapToStatusBatch creates a status batch from a given block.
func mapToStatusBatch(block *common.Block) []metrics.TxStatus {
	if block.Data == nil || len(block.Data.Data) == 0 {
		return nil
	}
	blockSize := len(block.Data.Data)

	var statusCodes []byte
	if block.Metadata != nil && len(block.Metadata.Metadata) > statusIdx {
		statusCodes = block.Metadata.Metadata[statusIdx]
	}
	logger.Infof("Received block #%d with %d TXs and %d statuses [%s]",
		block.Header.Number, len(block.Data.Data), len(statusCodes), recapStatusCodes(statusCodes),
	)

	statusBatch := make([]metrics.TxStatus, 0, blockSize)
	for i, data := range block.Data.Data {
		_, channelHeader, err := serialization.UnwrapEnvelope(data)
		if err != nil {
			logger.Warnf("Failed to unmarshal envelope: %v", err)
			continue
		}
		if common.HeaderType(channelHeader.Type) == common.HeaderType_CONFIG {
			// We can ignore config transactions as we only count data transactions.
			continue
		}
		status := committerpb.Status_COMMITTED
		if len(statusCodes) > i {
			status = committerpb.Status(statusCodes[i])
		}
		statusBatch = append(statusBatch, metrics.TxStatus{
			TxID:   channelHeader.TxId,
			Status: status,
		})
	}
	return statusBatch
}

// recapStatusCodes recaps of the status codes of a block.
func recapStatusCodes(statusCodes []byte) string {
	codes := utils.CountAppearances(statusCodes)
	items := make([]string, 0, len(codes))
	for code, count := range codes {
		items = append(
			items,
			fmt.Sprintf("%s x %d", committerpb.Status(code).String(), count),
		)
	}
	return strings.Join(items, ", ")
}
