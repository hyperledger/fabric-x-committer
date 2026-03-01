/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package sidecar

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-lib-go/common/flogging"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-protos-go-apiv2/peer"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/hyperledger/fabric-x-common/protoutil"
	"github.com/hyperledger/fabric-x-common/tools/configtxgen"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/utils"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/deliver"
	"github.com/hyperledger/fabric-x-committer/utils/dynamictls"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring/promutil"
	"github.com/hyperledger/fabric-x-committer/utils/ordererconn"
)

var logger = flogging.MustGetLogger("sidecar")

// Service is a relay service which relays the block from orderer to committer. Further,
// it aggregates the transaction status and forwards the validated block to clients who have
// registered on the ledger server.
type Service struct {
	ordererClient      *deliver.Client
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

	// dynamicCACerts holds the CA certificates for the sidecar's gRPC server.
	dynamicCACerts atomic.Pointer[[][]byte] // *[][]byte
}

// New creates a sidecar service.
func New(c *Config) (*Service, error) {
	logger.Info("Initializing new sidecar")

	// 1. Fetch blocks from the ordering service.
	ordererClient, err := deliver.New(&c.Orderer)
	if err != nil {
		return nil, fmt.Errorf("failed to create orderer client: %w", err)
	}

	// 2. Relay the blocks to committer and receive the transaction status.
	metrics := newPerformanceMetrics()
	relayService := newRelay(c.LastCommittedBlockSetInterval, metrics)

	// 3. Deliver the block with status to client.
	logger.Infof("Create block store for channel %s", c.Orderer.ChannelID)
	blockStore, err := newBlockStore(c.Orderer.ChannelID, c.Ledger.Path, c.Ledger.SyncInterval, metrics)
	if err != nil {
		return nil, fmt.Errorf("failed to create block store: %w", err)
	}

	if c.ChannelBufferSize <= 0 {
		c.ChannelBufferSize = defaultBufferSize
	}

	sidecarService := &Service{
		ordererClient:  ordererClient,
		relay:          relayService,
		notifier:       newNotifier(c.ChannelBufferSize, &c.Notification),
		blockStore:     blockStore,
		blockDelivery:  newBlockDelivery(blockStore, c.Orderer.ChannelID),
		blockQuery:     newBlockQuery(blockStore.store),
		healthcheck:    connection.DefaultHealthCheckService(),
		config:         c,
		metrics:        metrics,
		committedBlock: make(chan *common.Block, c.ChannelBufferSize),
		statusQueue:    make(chan []*committerpb.TxStatus, c.ChannelBufferSize),
		dynamicCACerts: atomic.Pointer[[][]byte]{},
	}

	if c.Bootstrap.GenesisBlockFilePath != "" {
		logger.Debug("updating orderer connections from genesis-block")
		configBlock, bootErr := configtxgen.ReadBlock(c.Bootstrap.GenesisBlockFilePath)
		if bootErr != nil {
			return nil, errors.Wrap(bootErr, "read config block")
		}
		orgsMaterial, bootErr := ordererconn.NewOrganizationsMaterialsFromConfigBlock(configBlock)
		if bootErr != nil {
			return nil, errors.Wrap(bootErr, "failed to load organizations materials")
		}
		bootErr = ordererClient.UpdateConnections(orgsMaterial)
		if bootErr != nil {
			return nil, bootErr
		}

		// reading application root CAs and add it to the YAML root CAs setup.
		logger.Debug("updating acceptable client CAs from genesis-block")
		rootCAs, bootErr := dynamictls.NewApplicationRootCAsFromConfigBlock(configBlock)
		if bootErr != nil {
			return nil, errors.Wrap(bootErr, "failed to load application root CAs")
		}
		sidecarService.updateCACertificates(rootCAs)
	}

	return sidecarService, nil
}

// GetDynamicRootCAs returns a pointer to the atomic pointer containing dynamic root CA certificates.
// The sidecar updates these CAs when processing config blocks from the orderer, enabling
// certificate rotation without a service restart.
func (s *Service) GetDynamicRootCAs() *atomic.Pointer[[][]byte] {
	return &s.dynamicCACerts
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
		return connection.Sustain(gCtx, func() error {
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

	// NOTE: ordererClient.Deliver and relay.Run must always return an error on exist.
	g.Go(func() error {
		logger.Info("Fetch blocks from the ordering service and write them on s.blockToBeCommitted.")
		deliverErr := s.ordererClient.Deliver(gCtx, &deliver.Parameters{
			NextBlockNum: nextBlockNum,
			OutputBlock:  s.blockToBeCommitted,
		})
		if errors.Is(deliverErr, context.Canceled) {
			// A context may be cancelled due to a relay error, thus it is not critical error.
			return errors.Wrap(deliverErr, "context is canceled")
		}
		return errors.Join(connection.ErrNonRetryable, deliverErr)
	})

	g.Go(func() error {
		logger.Info("Relay the blocks to committer (from s.blockToBeCommitted) and receive the transaction status.")
		return s.relay.run(gCtx, &relayRunConfig{
			coordClient:                    coordClient,
			nextExpectedBlockByCoordinator: nextBlockNum,
			configUpdater:                  s.configUpdater,
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

func (s *Service) configUpdater(block *common.Block) {
	logger.Infof("updating config from block: %d", block.Header.Number)
	orgsMaterial, err := ordererconn.NewOrganizationsMaterialsFromConfigBlock(block)
	if err != nil {
		logger.Warnf("failed to load config from block %d: %v", block.Header.Number, err)
		return
	}
	err = s.ordererClient.UpdateConnections(orgsMaterial)
	if err != nil {
		logger.Warnf("failed to update config for block %d: %v", block.Header.Number, err)
	}

	logger.Debug("updating sidecar's acceptable CAs from the config-block")
	rootCAs, err := dynamictls.NewApplicationRootCAsFromConfigBlock(block)
	if err != nil {
		logger.Warnf("failed to load CA certificates from block %d: %v", block.Header.Number, err)
	}
	s.updateCACertificates(rootCAs)
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

	// We should update the orderer endpoints first, before recovering the ledger
	// store. Otherwise, the `recoverLedgerStore` function might try to fetch blocks
	// from non-existent or non-member ordering services.
	if err := s.recoverConfigTransactionFromStateDB(ctx, coordClient); err != nil {
		return 0, err
	}

	blkInfo, err := coordClient.GetNextBlockNumberToCommit(ctx, nil)
	if err != nil {
		return 0, logAndWrapCoordinatorError(err, "failed to fetch the next expected block number from coordinator")
	}
	logger.Infof("next expected block number by coordinator is %d", blkInfo.Number)

	return blkInfo.Number, s.recoverLedgerStore(ctx, coordClient, blkInfo.Number)
}

func (s *Service) recoverConfigTransactionFromStateDB(
	ctx context.Context, client servicepb.CoordinatorClient,
) error {
	configMsg, err := client.GetConfigTransaction(ctx, nil)
	if err != nil {
		return logAndWrapCoordinatorError(err, "failed to get config transaction from coordinator")
	}
	if configMsg == nil || configMsg.Envelope == nil {
		return nil
	}
	envelope, err := protoutil.UnmarshalEnvelope(configMsg.Envelope)
	if err != nil {
		return fmt.Errorf("failed to unmarshal meta policy envelope: %w", err)
	}
	orgsMaterial, err := ordererconn.NewOrganizationsMaterialsFromEnvelope(envelope)
	if err != nil {
		return err
	}
	if err = s.ordererClient.UpdateConnections(orgsMaterial); err != nil {
		return errors.Wrapf(err, "failed to update connections")
	}
	logger.Debug("updating sidecar's acceptable CAs from the database")
	rootCAs, err := dynamictls.NewOrganizationsFromEnvelope(envelope)
	if err != nil {
		return err
	}
	s.updateCACertificates(rootCAs)
	return nil
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
		logger.Infof("starting delivery service with the orderer to receive block %d", blockStoreHeight)
		deliverErr := s.ordererClient.Deliver(gCtx, &deliver.Parameters{
			NextBlockNum: blockStoreHeight,
			OutputBlock:  blockCh,
		})
		if errors.Is(deliverErr, context.Canceled) {
			return nil
		}
		return deliverErr
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

// updateCACertificates atomically updates the dynamic root CA certificates.
func (s *Service) updateCACertificates(newCAs [][]byte) {
	if len(newCAs) == 0 {
		logger.Warn("no CA certificates found in config block")
	}

	// Atomically update the CA certificates
	s.dynamicCACerts.Store(&newCAs)
	logger.Debugf("updated %d CA certificates", len(newCAs))
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
