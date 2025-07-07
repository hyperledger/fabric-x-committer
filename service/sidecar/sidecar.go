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
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-protos-go-apiv2/peer"
	"github.com/hyperledger/fabric/protoutil"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
	"github.com/hyperledger/fabric-x-committer/api/protocoordinatorservice"
	"github.com/hyperledger/fabric-x-committer/api/types"
	"github.com/hyperledger/fabric-x-committer/utils"
	"github.com/hyperledger/fabric-x-committer/utils/broadcastdeliver"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/logging"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring/promutil"
)

var logger = logging.New("sidecar")

// Service is a relay service which relays the block from orderer to committer. Further,
// it aggregates the transaction status and forwards the validated block to clients who have
// registered on the ledger server.
type Service struct {
	ordererClient      *broadcastdeliver.Client
	relay              *relay
	ledgerService      *LedgerService
	coordConn          *grpc.ClientConn
	blockToBeCommitted chan *common.Block
	committedBlock     chan *common.Block
	config             *Config
	healthcheck        *health.Server
	metrics            *perfMetrics
}

// New creates a sidecar service.
func New(c *Config) (*Service, error) {
	logger.Info("Initializing new sidecar")
	err := LoadBootstrapConfig(c)
	if err != nil {
		return nil, fmt.Errorf("failed to load shared config: %w", err)
	}

	// 1. Fetch blocks from the ordering service.
	ordererClient, err := broadcastdeliver.New(&c.Orderer)
	if err != nil {
		return nil, fmt.Errorf("failed to create orderer client: %w", err)
	}

	// 2. Relay the blocks to committer and receive the transaction status.
	metrics := newPerformanceMetrics()
	relayService := newRelay(c.LastCommittedBlockSetInterval, metrics)

	// 3. Deliver the block with status to client.
	logger.Infof("Create ledger service for channel %s", c.Orderer.ChannelID)
	ledgerService, err := newLedgerService(c.Orderer.ChannelID, c.Ledger.Path, metrics)
	if err != nil {
		return nil, fmt.Errorf("failed to create ledger: %w", err)
	}
	return &Service{
		ordererClient:      ordererClient,
		relay:              relayService,
		ledgerService:      ledgerService,
		healthcheck:        connection.DefaultHealthCheckService(),
		config:             c,
		metrics:            metrics,
		blockToBeCommitted: make(chan *common.Block, 100),
		committedBlock:     make(chan *common.Block, 100),
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
		_ = s.metrics.StartPrometheusServer(pCtx, s.config.Monitoring.Server, s.monitorQueues)
	}()

	logger.Infof("Create coordinator client and connect to %s\n", &s.config.Committer.Endpoint)
	conn, connErr := connection.Connect(connection.NewInsecureDialConfig(&s.config.Committer.Endpoint))
	if connErr != nil {
		return fmt.Errorf("failed to connect to coordinator: %w", connErr)
	}
	s.coordConn = conn
	defer connection.CloseConnectionsLog(conn)
	logger.Infof("sidecar connected to coordinator at %s", &s.config.Committer.Endpoint)
	coordClient := protocoordinatorservice.NewCoordinatorClient(conn)

	g, gCtx := errgroup.WithContext(pCtx)

	// Deliver the block with status to client runs independently of the coordinator connection lifecycle.
	// eCtx will be cancelled if this service stopped processing the blocks due to ledger error.
	// Such errors require human interaction to resolve the ledger discrepancy.
	g.Go(func() error {
		return s.ledgerService.run(gCtx, &ledgerRunConfig{
			IncomingCommittedBlock: s.committedBlock,
		})
	})

	g.Go(func() error {
		return connection.Sustain(gCtx, func() error {
			defer func() {
				s.recoverCommittedBlocks(gCtx)
				s.blockToBeCommitted = make(chan *common.Block, 100) // We should drop all enqueued block if any.
			}()
			return s.sendBlocksAndReceiveStatus(gCtx, coordClient)
		})
	})

	return utils.ProcessErr(g.Wait(), "sidecar has been stopped")
}

// RegisterService registers for the sidecar's GRPC services.
func (s *Service) RegisterService(server *grpc.Server) {
	peer.RegisterDeliverServer(server, s.ledgerService)
	healthgrpc.RegisterHealthServer(server, s.healthcheck)
}

func (s *Service) sendBlocksAndReceiveStatus(
	ctx context.Context,
	coordClient protocoordinatorservice.CoordinatorClient,
) error {
	defer s.metrics.coordConnection.Disconnected(s.coordConn.CanonicalTarget())
	nextBlockNum, err := s.recover(ctx, coordClient)
	if err != nil {
		return errors.Join(connection.ErrBackOff, err)
	}

	// if the recovery is successful, the connection is established.
	s.metrics.coordConnection.Connected(s.coordConn.CanonicalTarget())

	g, gCtx := errgroup.WithContext(ctx)

	// NOTE: ordererClient.Deliver and relay.Run must always return an error on exist.
	g.Go(func() error {
		logger.Info("Fetch blocks from the ordering service and write them on s.blockToBeCommitted.")
		err := s.ordererClient.Deliver(gCtx, &broadcastdeliver.DeliverConfig{
			StartBlkNum: int64(nextBlockNum), //nolint:gosec
			EndBlkNum:   broadcastdeliver.MaxBlockNum,
			OutputBlock: s.blockToBeCommitted,
		})
		if errors.Is(err, context.Canceled) {
			// A context may be cancelled due to a relay error, thus it is not critical error.
			return errors.Wrap(err, "context is canceled")
		}
		return errors.Join(connection.ErrNonRetryable, err)
	})

	g.Go(func() error {
		logger.Info("Relay the blocks to committer (from s.blockToBeCommitted) and receive the transaction status.")
		return s.relay.run(gCtx, &relayRunConfig{
			coordClient:                    coordClient,
			nextExpectedBlockByCoordinator: nextBlockNum,
			configUpdater:                  s.configUpdater,
			incomingBlockToBeCommitted:     s.blockToBeCommitted,
			outgoingCommittedBlock:         s.committedBlock,
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

func (s *Service) configUpdater(block *common.Block) {
	logger.Infof("updating config from block: %d", block.Header.Number)
	err := OverwriteConfigFromBlock(s.config, block)
	if err != nil {
		logger.Warnf("failed to load config from block %d: %v", block.Header.Number, err)
		return
	}
	err = s.ordererClient.UpdateConnections(&s.config.Orderer.Connection)
	if err != nil {
		logger.Warnf("failed to update config for block %d: %v", block.Header.Number, err)
	}
}

func (s *Service) recover(ctx context.Context, coordClient protocoordinatorservice.CoordinatorClient) (uint64, error) {
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

	// We should update the orderer endpoints first, before recovering the ledger
	// store. Otherwise, the `recoverLedgerStore` function might try to fetch blocks
	// from non-existent or non-member ordering services.
	if err := s.recoverConfigTransactionFromStateDB(ctx, coordClient); err != nil {
		return 0, err
	}

	blkInfo, err := coordClient.GetNextExpectedBlockNumber(ctx, nil)
	if err != nil {
		return 0, errors.Wrap(err, "failed to fetch the next expected block number from coordinator")
	}
	logger.Infof("next expected block number by coordinator is %d", blkInfo.Number)

	return blkInfo.Number, s.recoverLedgerStore(ctx, coordClient, blkInfo.Number)
}

func (s *Service) recoverConfigTransactionFromStateDB(
	ctx context.Context, client protocoordinatorservice.CoordinatorClient,
) error {
	configMsg, err := client.GetConfigTransaction(ctx, nil)
	if err != nil {
		return errors.Wrapf(err, "failed to get policies from coordinator")
	}
	if configMsg == nil || configMsg.Envelope == nil {
		return nil
	}
	envelope, err := protoutil.UnmarshalEnvelope(configMsg.Envelope)
	if err != nil {
		return fmt.Errorf("failed to unmarshal meta policy envelope: %w", err)
	}
	err = OverwriteConfigFromEnvelope(s.config, envelope)
	if err != nil {
		return err
	}
	err = s.ordererClient.UpdateConnections(&s.config.Orderer.Connection)
	return errors.Wrapf(err, "failed to update connections")
}

func (s *Service) recoverLedgerStore(
	ctx context.Context,
	client protocoordinatorservice.CoordinatorClient,
	stateDBHeight uint64,
) error {
	blockStoreHeight := s.ledgerService.GetBlockHeight()
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

	g, gCtx := errgroup.WithContext(ctx)
	blockCh := make(chan *common.Block, numOfBlocksPendingInBlockStore)
	committedBlocks := channel.NewWriter(ctx, s.committedBlock)

	g.Go(func() error {
		logger.Infof("starting delivery service with the orderer to receive block %d to %d",
			blockStoreHeight, stateDBHeight-1)
		return s.ordererClient.Deliver(gCtx, &broadcastdeliver.DeliverConfig{
			StartBlkNum: int64(blockStoreHeight), //nolint:gosec
			EndBlkNum:   stateDBHeight - 1,
			OutputBlock: blockCh,
		})
	})

	g.Go(func() error {
		for range numOfBlocksPendingInBlockStore {
			select {
			case <-gCtx.Done():
				return gCtx.Err()
			case blk := <-blockCh:
				if err := appendMissingBlock(gCtx, client, blk, committedBlocks); err != nil {
					return err
				}
			}
		}
		logger.Infof("successfully recover ledger store by adding [%d] missing blocks", numOfBlocksPendingInBlockStore)
		return nil
	})

	return errors.Wrap(g.Wait(), "failed to recover the ledger store")
}

func appendMissingBlock(
	ctx context.Context,
	client protocoordinatorservice.CoordinatorClient,
	blk *common.Block,
	committedBlocks channel.Writer[*common.Block],
) error {
	var txIDToHeight utils.SyncMap[string, types.Height]
	mappedBlock := mapBlock(blk, &txIDToHeight)
	txIDs := make([]string, len(mappedBlock.block.Txs))
	expectedHeight := make(map[string]*types.Height)
	for i, tx := range mappedBlock.block.Txs {
		txIDs[i] = tx.Id
		expectedHeight[tx.Id] = types.NewHeight(mappedBlock.block.Number, mappedBlock.block.TxsNum[i])
	}

	txsStatus, err := client.GetTransactionsStatus(ctx, &protoblocktx.QueryStatus{TxIDs: txIDs})
	if err != nil {
		return errors.Wrap(err, "failed to get transaction status from the coordinator")
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
	s.ledgerService.close()
}

// GetLedgerService returns the ledger that implements peer.DeliverServer.
func (s *Service) GetLedgerService() *LedgerService {
	return s.ledgerService
}

func waitForIdleCoordinator(ctx context.Context, client protocoordinatorservice.CoordinatorClient) error {
	for {
		waitingTxs, err := client.NumberOfWaitingTransactionsForStatus(ctx, nil)
		if err != nil {
			return errors.Wrap(err, "failed to get the number of transactions waiting in the coordinator for statuses")
		}
		if waitingTxs.Count == 0 {
			return nil
		}
		logger.Infof("Waiting for coordinator to complete processing [%d] pending transactions", waitingTxs.Count)
		time.Sleep(100 * time.Millisecond)
	}
}

func fillStatuses(
	finalStatuses []protoblocktx.Status,
	statuses map[string]*protoblocktx.StatusWithHeight,
	expectedHeight map[string]*types.Height,
) error {
	for txID, height := range expectedHeight {
		s, ok := statuses[txID]
		if !ok {
			return errors.Newf("committer should have the status of txID [%s] but it does not", txID)
		}
		if types.AreSame(height, types.NewHeight(s.BlockNumber, s.TxNumber)) {
			finalStatuses[height.TxNum] = s.Code
			continue
		}
		finalStatuses[height.TxNum] = protoblocktx.Status_REJECTED_DUPLICATE_TX_ID
	}
	return nil
}
