/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package sidecar

import (
	"context"
	"fmt"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-lib-go/common/metrics/disabled"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-x-common/common/ledger/blkstorage"
	"github.com/hyperledger/fabric-x-common/common/ledger/blockledger/fileledger"

	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring/promutil"
)

type (
	// blockStore manages block persistence and height tracking.
	blockStore struct {
		ledger                       *fileledger.FileLedger
		store                        *blkstorage.BlockStore
		storeProvider                *blkstorage.BlockStoreProvider
		channelID                    string
		nextToBeCommittedBlockNumber uint64
		syncInterval                 uint64
		metrics                      *perfMetrics
	}

	// blockStoreRunConfig holds the configuration needed to run the block store.
	blockStoreRunConfig struct {
		IncomingCommittedBlock <-chan *common.Block
	}
)

// newBlockStore creates a new block store.
func newBlockStore(channelID, ledgerDir string, syncInterval uint64, metrics *perfMetrics) (*blockStore, error) {
	logger.Infof("Create block store for channel %s under %s", channelID, ledgerDir)

	provider, err := blkstorage.NewProvider(
		blkstorage.NewConf(ledgerDir, -1),
		&blkstorage.IndexConfig{
			AttrsToIndex: []blkstorage.IndexableAttr{
				blkstorage.IndexableAttrBlockNum,
				blkstorage.IndexableAttrTxID,
			},
		},
		&disabled.Provider{},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create block store provider: %w", err)
	}

	store, err := provider.Open(channelID)
	if err != nil {
		return nil, fmt.Errorf("failed to open block store for channel %s: %w", channelID, err)
	}

	ledger := fileledger.NewFileLedger(store)

	return &blockStore{
		ledger:                       ledger,
		store:                        store,
		storeProvider:                provider,
		channelID:                    channelID,
		nextToBeCommittedBlockNumber: ledger.Height(),
		syncInterval:                 syncInterval,
		metrics:                      metrics,
	}, nil
}

// run starts the block store. The call to run blocks until an error occurs or the context is canceled.
// When syncInterval > 1, intermediate blocks are written without fsync (AppendNoSync) and every
// Nth block triggers a full sync (Append). This reduces per-block fsync overhead while bounding
// the number of blocks that could be lost on crash (recoverable from the orderer).
// File rollovers always force a sync regardless of the interval (handled in the block store).
func (s *blockStore) run(ctx context.Context, config *blockStoreRunConfig) error {
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
		if err := s.appendBlock(block); err != nil {
			return err
		}
		promutil.Observe(s.metrics.appendBlockToLedgerSeconds, time.Since(start))
		promutil.SetUint64Gauge(s.metrics.blockHeight, block.Header.Number+1)
		logger.Debugf("Appended block %d to ledger.", block.Header.Number)
	}
}

// appendBlock appends a block to the ledger. It uses AppendNoSync for intermediate
// blocks and Append (with fsync) every syncInterval blocks.
func (s *blockStore) appendBlock(block *common.Block) error {
	if s.syncInterval <= 1 || (block.Header.Number+1)%s.syncInterval == 0 {
		return s.ledger.Append(block)
	}
	return s.ledger.AppendNoSync(block)
}

// close releases the ledger directory.
func (s *blockStore) close() {
	s.storeProvider.Close()
}

// GetBlockHeight returns the height of the block store, i.e., the last committed block + 1. The +1 is needed
// to include block 0 as well.
func (s *blockStore) GetBlockHeight() uint64 {
	return s.ledger.Height()
}
