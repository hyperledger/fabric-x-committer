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
	"golang.org/x/sync/errgroup"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
	"github.com/hyperledger/fabric-x-committer/loadgen/metrics"
	"github.com/hyperledger/fabric-x-committer/service/sidecar/sidecarclient"
	"github.com/hyperledger/fabric-x-committer/utils/broadcastdeliver"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/serialization"
)

type receiverConfig struct {
	Endpoint  *connection.Endpoint
	ChannelID string
	Res       *ClientResources
}

const committedBlocksQueueSize = 1024

// runReceiver start receiving blocks from the sidecar.
func runReceiver(ctx context.Context, config *receiverConfig) error {
	ledgerReceiver, err := sidecarclient.New(&sidecarclient.Config{
		ChannelID: config.ChannelID,
		Endpoint:  config.Endpoint,
	})
	if err != nil {
		return errors.Wrap(err, "failed to create ledger receiver")
	}

	g, gCtx := errgroup.WithContext(ctx)
	committedBlock := make(chan *common.Block, committedBlocksQueueSize)
	g.Go(func() error {
		return ledgerReceiver.Deliver(gCtx, &sidecarclient.DeliverConfig{
			EndBlkNum:   broadcastdeliver.MaxBlockNum,
			OutputBlock: committedBlock,
		})
	})
	g.Go(func() error {
		receiveCommittedBlock(gCtx, committedBlock, config.Res)
		return context.Canceled
	})
	return errors.Wrap(g.Wait(), "sidecar receiver done")
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

			statusCodes := block.Metadata.Metadata[common.BlockMetadataIndex_TRANSACTIONS_FILTER]
			logger.Infof("Received block #%d with %d TXs and %d statuses [%s]",
				block.Header.Number, len(block.Data.Data), len(statusCodes), recapStatusCodes(statusCodes),
			)

			statusBatch := make([]metrics.TxStatus, 0, len(block.Data.Data))
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
				statusBatch = append(statusBatch, metrics.TxStatus{
					TxID:   channelHeader.TxId,
					Status: protoblocktx.Status(statusCodes[i]),
				})
			}
			processedBlocks.Write(statusBatch)
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

// recapStatusCodes recaps of the status codes of a block.
func recapStatusCodes(statusCodes []byte) string {
	codes := make(map[byte]uint64)
	for _, code := range statusCodes {
		codes[code]++
	}
	items := make([]string, 0, len(codes))
	for code, count := range codes {
		items = append(
			items,
			fmt.Sprintf("%s x %d", protoblocktx.Status(code).String(), count),
		)
	}
	return strings.Join(items, ", ")
}
