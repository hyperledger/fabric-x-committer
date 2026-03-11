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
	"github.com/hyperledger/fabric-lib-go/common/flogging"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-protos-go-apiv2/peer"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/hyperledger/fabric-x-common/protoutil"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/utils"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/deliverorderer"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring/promutil"
)

var logger = flogging.MustGetLogger("sidecar")

// Service is a relay service which relays the block from orderer to committer. Further,
// it aggregates the transaction status and forwards the validated block to clients who have
// registered on the ledger server.
type Service struct {
	deliveryParams     *deliverorderer.Parameters
	relay              *relay
	notifier           *notifier
	blockStore         *blockStore
	blockDelivery      *blockDelivery
	blockQuery         *blockQuery
	coordConn          *grpc.ClientConn
	blockToBeCommitted chan *common.Block
	committedBlock     chan *common.Block
	statusQueue        chan []*committerpb.TxStatus
	config             *Config
	healthcheck        *health.Server
	metrics            *perfMetrics
}

// New creates a sidecar service.
func New(c *Config) (*Service, error) {
	logger.Info("Initializing new sidecar")

	// 1. Load the delivery parameters for the ordering service.
	deliveryParams, err := deliverorderer.LoadParametersFromConfig(&c.Orderer)
	if err != nil {
		return nil, err
	}

	// 2. Relay the blocks to committer and receive the transaction status.
	metrics := newPerformanceMetrics()
	relayService := newRelay(c.LastCommittedBlockSetInterval, metrics)

	// 3. Deliver the block with status to client.
	blockStoreInstance, err := newBlockStore(c.Ledger.Path, c.Ledger.SyncInterval, metrics)
	if err != nil {
		return nil, fmt.Errorf("failed to create block store: %w", err)
	}

	if c.ChannelBufferSize <= 0 {
		c.ChannelBufferSize = defaultBufferSize
	}
	return &Service{
		deliveryParams: deliveryParams,
		relay:          relayService,
		notifier:       newNotifier(c.ChannelBufferSize, &c.Notification),
		blockStore:     blockStoreInstance,
		blockDelivery:  newBlockDelivery(blockStoreInstance),
		blockQuery:     newBlockQuery(blockStoreInstance),
		healthcheck:    connection.DefaultHealthCheckService(),
		config:         c,
		metrics:        metrics,
		committedBlock: make(chan *common.Block, c.ChannelBufferSize),
		statusQueue:    make(chan []*committerpb.TxStatus, c.ChannelBufferSize),
	}, nil
}

// WaitForReady wait for sidecar to be ready to be exposed as gRPC service.
// If the context ended before the service is ready, returns false.
func (*Service) WaitForReady(context.Context) bool {
	return true
}

// Run starts the sidecar service. The call to Run blocks until an error occurs or the context is canceled.
func (s *Service) Run(ctx context.Context) error {
	pCtx, pCancel := context.WithCancel(ctx)
	defer pCancel()
	// similar to other services, when the prometheus server returns an error, we do not terminate the service.
	go func() {
		_ = s.metrics.StartPrometheusServer(pCtx, s.config.Monitoring, s.monitorQueues)
	}()

	logger.Infof("Create coordinator client and connect to %s", s.config.Committer.Endpoint)
	conn, connErr := connection.NewSingleConnection(s.config.Committer)
	if connErr != nil {
		return errors.Wrapf(connErr, "failed to connect to coordinator")
	}
	s.coordConn = conn
	defer connection.CloseConnectionsLog(conn)
	logger.Infof("sidecar connected to coordinator at %s", s.config.Committer.Endpoint)
	coordClient := servicepb.NewCoordinatorClient(conn)

	g, gCtx := errgroup.WithContext(pCtx)

	// The following runs independently of the coordinator connection lifecycle.
	// gCtx will be cancelled if these stopped processing due to ledger error.
	// Such errors require human interaction to resolve the ledger discrepancy.
	g.Go(func() error {
		// Deliver the block with status to clients.
		return s.blockStore.run(gCtx, &blockStoreRunConfig{
			IncomingCommittedBlock: s.committedBlock,
		})
	})
	g.Go(func() error {
		// Notification for clients.
		return s.notifier.run(gCtx, s.statusQueue)
	})

	g.Go(func() error {
		// TODO: initialize retry from config.
		return connection.Sustain(gCtx, nil, func() error {
			defer func() {
				s.recoverCommittedBlocks(gCtx)
			}()
			return s.sendBlocksAndReceiveStatus(gCtx, coordClient)
		})
	})

	return utils.ProcessErr(g.Wait(), "sidecar has been stopped")
}

// RegisterService registers for the sidecar's GRPC services.
func (s *Service) RegisterService(server *grpc.Server) {
	peer.RegisterDeliverServer(server, s.blockDelivery)
	committerpb.RegisterNotifierServer(server, s.notifier)
	committerpb.RegisterBlockQueryServiceServer(server, s.blockQuery)
	healthgrpc.RegisterHealthServer(server, s.healthcheck)
}

func (s *Service) sendBlocksAndReceiveStatus(
	ctx context.Context,
	coordClient servicepb.CoordinatorClient,
) error {
	defer s.metrics.coordConnection.Disconnected(s.coordConn.CanonicalTarget())
	nextBlockNum, err := s.recover(ctx, coordClient)
	if err != nil {
		return errors.Join(connection.ErrBackOff, err)
	}

	// if the recovery is successful, the connection is established.
	s.metrics.coordConnection.Connected(s.coordConn.CanonicalTarget())

	g, gCtx := errgroup.WithContext(ctx)

	// We drop all enqueued block if any before starting a new session.
	s.blockToBeCommitted = make(chan *common.Block, s.config.ChannelBufferSize)

	// NOTE: deliver.OrdererToChannel and relay.Run must always return an error on exist.
	g.Go(func() error {
		return s.startDelivery(gCtx, s.blockToBeCommitted, nextBlockNum)
	})

	g.Go(func() error {
		logger.Info("Relay the blocks to committer (from s.blockToBeCommitted) and receive the transaction status.")
		return s.relay.run(gCtx, &relayRunConfig{
			coordClient:                    coordClient,
			nextExpectedBlockByCoordinator: nextBlockNum,
			incomingBlockToBeCommitted:     s.blockToBeCommitted,
			outgoingCommittedBlock:         s.committedBlock,
			outgoingStatusUpdates:          s.statusQueue,
			waitingTxsLimit:                s.config.WaitingTxsLimit,
		})
	})

	return g.Wait()
}

func (s *Service) recoverCommittedBlocks(ctx context.Context) {
	for ctx.Err() == nil && len(s.committedBlock) > 0 {
		logger.Infof("Waiting for committed block queue: %d", len(s.committedBlock))
		time.Sleep(100 * time.Millisecond)
	}
}

func (s *Service) recover(ctx context.Context, coordClient servicepb.CoordinatorClient) (uint64, error) {
	logger.Info("recovering sidecar")
	// If the sidecar fails but the coordinator remains active, the sidecar
	// must wait for the coordinator to become idle (i.e., to finish
	// processing all previously submitted transactions) before attempting
	// recovery. This ensures proper block store recovery in the sidecar.
	// For example, suppose blocks 100 through 200 were sent to the coordinator.
	// Some of these blocks might be partially committed, with the sidecar
	// having received partial status updates, when the sidecar crashes and
	// restarts. Upon restart, the sidecar can easily determine the
	// coordinator's next expected block. However, it needs to reconstruct
	// its block store to fill any gaps.
	// The sidecar can identify missing blocks using its current block store
	// height and the coordinator's next expected block number. It can then
	// fetch these missing blocks from the ordering service and their statuses
	// from the coordinator, finally committing them to its local block store.
	// However, if the coordinator has not fully committed these blocks,
	// the sidecar might not be able to retrieve all necessary status updates.
	// To prevent this, the sidecar waits for the coordinator to complete all
	// pending transactions before attempting recovery. This ensures that the
	// sidecar retrieves complete status information and avoids inconsistencies
	// in its block store.
	if err := waitForIdleCoordinator(ctx, coordClient); err != nil {
		return 0, err
	}

	blkInfo, err := coordClient.GetNextBlockNumberToCommit(ctx, nil)
	if err != nil {
		return 0, logAndWrapCoordinatorError(err, "failed to fetch the next expected block number from coordinator")
	}
	logger.Infof("Next expected block number by coordinator is %d", blkInfo.Number)

	return blkInfo.Number, s.recoverLedgerStore(ctx, coordClient, blkInfo.Number)
}

func (s *Service) recoverLedgerStore(
	ctx context.Context,
	client servicepb.CoordinatorClient,
	stateDBHeight uint64,
) error {
	blockStoreHeight := s.blockStore.GetBlockHeight()
	if blockStoreHeight >= stateDBHeight {
		// NOTE: The block store height can be greater than the state database height.
		//       This occurs because the last committed block number is updated in the state
		//       database periodically, whereas blocks are written to the block store immediately.
		//       Therefore, the block store height can temporarily exceed the state database height.
		return nil
	}

	numOfBlocksPendingInBlockStore := stateDBHeight - blockStoreHeight
	logger.Infof("ledger store is [%d] blocks behind the state database in the committer",
		numOfBlocksPendingInBlockStore)

	cCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	g, gCtx := errgroup.WithContext(cCtx)

	blockCh := make(chan *common.Block, numOfBlocksPendingInBlockStore)

	g.Go(func() error {
		return s.startDelivery(gCtx, blockCh, blockStoreHeight)
	})

	g.Go(func() error {
		defer cancel()
		blocks := channel.NewReader(gCtx, blockCh)
		committedBlocks := channel.NewWriter(ctx, s.committedBlock)
		for range numOfBlocksPendingInBlockStore {
			blk, ok := blocks.Read()
			if !ok {
				return gCtx.Err()
			}
			appendErr := appendMissingBlock(gCtx, client, blk, committedBlocks)
			if appendErr != nil {
				return appendErr
			}
		}
		logger.Infof("successfully recover ledger store by adding [%d] missing blocks", numOfBlocksPendingInBlockStore)
		return nil
	})

	return errors.Wrap(g.Wait(), "failed to recover the ledger store")
}

// startDelivery fetches blocks from the ordering service and write them to a channel.
func (s *Service) startDelivery(ctx context.Context, output chan<- *common.Block, nextBlockNum uint64) error {
	_, latestConfig, err := s.getPrevBlockAndItsConfig(s.blockStore.GetBlockHeight())
	if err != nil {
		return err
	}

	// We use the maximum between the latest config block in the ledger, the config block from the YAML,
	// and the latest from the previous delivery runs.
	// This ensures we won't miss a crucial config-block that updated all the endpoints and/or the credentials.
	lastestKnownConfig := maxBlock(s.deliveryParams.Session.LastestKnownConfig, latestConfig)
	lastBlock, nextBlockVerificationConfig, err := s.getPrevBlockAndItsConfig(nextBlockNum)
	if err != nil {
		return err
	}

	p := *s.deliveryParams
	p.OutputBlock = output
	p.Session = &deliverorderer.SessionInfo{
		LastestKnownConfig:          lastestKnownConfig,
		LastBlock:                   lastBlock,
		NextBlockVerificationConfig: nextBlockVerificationConfig,
	}
	logger.Infof("Staring delivery from block [%d]", nextBlockNum)
	deliverErr := deliverorderer.ToQueue(ctx, p)

	// We keep the session from previous runs.
	s.deliveryParams.Session = p.Session

	if errors.Is(deliverErr, context.Canceled) {
		// A context may be canceled due to a relay error, thus it is not critical error.
		return errors.Wrap(deliverErr, "context is canceled")
	}
	return errors.Join(connection.ErrNonRetryable, deliverErr)
}

func maxBlock(lBlk, rBlk *common.Block) *common.Block {
	if lBlk == nil || (rBlk != nil && rBlk.Header.Number > lBlk.Header.Number) {
		return rBlk
	}
	return lBlk
}

func (s *Service) getPrevBlockAndItsConfig(nextBlockNum uint64) (blk, configBlk *common.Block, err error) {
	if nextBlockNum == 0 {
		return nil, nil, nil
	}
	blk, err = s.blockStore.store.RetrieveBlockByNumber(nextBlockNum - 1)
	if err != nil {
		return nil, nil, err
	}
	configBlockIdx, err := protoutil.GetLastConfigIndexFromBlock(blk)
	if err != nil {
		return nil, nil, errors.Wrapf(err, "failed to get config index from block [%d]", nextBlockNum-1)
	}
	logger.Infof("Block [%d] config block number: %d", nextBlockNum-1, configBlockIdx)
	// Config blocks may point to itself.
	configBlk = blk
	if configBlockIdx != blk.Header.Number {
		configBlk, err = s.blockStore.store.RetrieveBlockByNumber(configBlockIdx)
		if err != nil {
			return nil, nil, errors.Wrapf(err, "failed to get config block [%d]", configBlockIdx)
		}
	}
	return blk, configBlk, nil
}

func appendMissingBlock(
	ctx context.Context,
	client servicepb.CoordinatorClient,
	blk *common.Block,
	committedBlocks channel.Writer[*common.Block],
) error {
	var txIDToHeight utils.SyncMap[string, servicepb.Height]
	mappedBlock, err := mapBlock(blk, &txIDToHeight)
	if err != nil {
		// This can never occur unless there is a bug in the relay.
		return err
	}

	txIDs := make([]string, len(mappedBlock.block.Txs))
	expectedHeight := make([]*committerpb.TxRef, len(mappedBlock.block.Txs))
	for i, tx := range mappedBlock.block.Txs {
		txIDs[i] = tx.Ref.TxId
		expectedHeight[i] = tx.Ref
	}

	txsStatus, err := client.GetTransactionsStatus(ctx, &committerpb.TxIDsBatch{TxIds: txIDs})
	if err != nil {
		return logAndWrapCoordinatorError(err, "failed to get transaction status from coordinator")
	}

	if err := fillStatuses(mappedBlock.withStatus.txStatus, txsStatus.Status, expectedHeight); err != nil {
		return err
	}

	mappedBlock.withStatus.setStatusMetadataInBlock()

	if !committedBlocks.Write(mappedBlock.withStatus.block) {
		return errors.New("context ended")
	}
	return nil
}

func (s *Service) monitorQueues(ctx context.Context) {
	ticker := time.NewTicker(100 * time.Millisecond)
	m := s.metrics
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		promutil.SetGauge(m.yetToBeCommittedBlocksQueueSize, len(s.blockToBeCommitted))
		promutil.SetGauge(m.committedBlocksQueueSize, len(s.committedBlock))
	}
}

// Close closes the ledger.
func (s *Service) Close() {
	s.blockStore.close()
}

func waitForIdleCoordinator(ctx context.Context, client servicepb.CoordinatorClient) error {
	for {
		waitingTxs, err := client.NumberOfWaitingTransactionsForStatus(ctx, nil)
		if err != nil {
			return logAndWrapCoordinatorError(err, "failed to get number of waiting transactions from coordinator")
		}
		if waitingTxs.Count == 0 {
			return nil
		}
		logger.Infof("Waiting for coordinator to complete processing [%d] pending transactions", waitingTxs.Count)
		time.Sleep(100 * time.Millisecond)
	}
}

func fillStatuses(
	finalStatuses []committerpb.Status,
	statuses []*committerpb.TxStatus,
	expectedHeight []*committerpb.TxRef,
) error {
	// This copy to a map has a negligible performance impact is fine as this is a rare case.
	statusMap := make(map[string]*committerpb.TxStatus, len(statuses))
	for _, s := range statuses {
		statusMap[s.Ref.TxId] = s
	}
	for _, ref := range expectedHeight {
		s, ok := statusMap[ref.TxId]
		if !ok {
			return errors.Newf("committer should have the status of txID [%s] but it does not", ref.TxId)
		}
		if committerpb.AreSameHeight(ref, s.Ref) {
			finalStatuses[ref.TxNum] = s.Status
		} else {
			finalStatuses[ref.TxNum] = committerpb.Status_REJECTED_DUPLICATE_TX_ID
		}
	}
	return nil
}

// logAndWrapCoordinatorError logs the gRPC status code from a coordinator error and wraps it with context.
// This helps with debugging by providing visibility into the specific gRPC error codes returned by the coordinator.
func logAndWrapCoordinatorError(err error, contextMsg string) error {
	if err == nil {
		return nil
	}

	st, ok := status.FromError(err)
	if ok {
		logger.Errorf("%s: gRPC status code=%s, message=%s", contextMsg, st.Code(), st.Message())
	} else {
		logger.Errorf("%s: %v", contextMsg, err)
	}

	return errors.Wrap(err, contextMsg)
}
