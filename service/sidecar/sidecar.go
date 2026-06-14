/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package sidecar

import (
	"context"
	"fmt"
	"io"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-lib-go/common/flogging"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	ab "github.com/hyperledger/fabric-protos-go-apiv2/orderer"
	"github.com/hyperledger/fabric-protos-go-apiv2/peer"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/hyperledger/fabric-x-common/common/ledger/blkstorage"
	"github.com/hyperledger/fabric-x-common/common/ledger/blockledger"
	"github.com/hyperledger/fabric-x-common/common/util"
	"github.com/hyperledger/fabric-x-common/protoutil"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/hyperledger/fabric-x-committer/utils/grpcerror"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/utils"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/deliverorderer"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring/promutil"
	"github.com/hyperledger/fabric-x-committer/utils/retry"
	"github.com/hyperledger/fabric-x-committer/utils/serialization"
	"github.com/hyperledger/fabric-x-committer/utils/serve"
)

var logger = flogging.MustGetLogger("sidecar")

// Service is a relay service which relays the block from orderer to committer. Further,
// it aggregates the transaction status and forwards the validated block to clients who have
// registered on the ledger server.
//   - Implements peer.DeliverServer by streaming blocks from a blockStore.
//   - Implements committerpb.BlockQueryServiceServer by delegating
//     read-only queries directly to the underlying block store.
type Service struct {
	committerpb.UnimplementedBlockQueryServiceServer
	deliveryParams        deliverorderer.Parameters
	relay                 *relay
	notifier              *notifier
	blockStore            *blockStore
	coordConn             *grpc.ClientConn
	blockToBeCommitted    atomic.Pointer[chan *common.Block]
	committedBlock        chan *common.Block
	committedBlockWithTxs chan *committedBlockWithTxs
	statusQueue           chan []*committerpb.TxStatus
	config                *Config
	healthcheck           *health.Server
	metrics               *perfMetrics
	tlsUpdater            serve.DynamicTLSUpdater
	ready                 *channel.Ready
}

var (
	blockReadyRetryProfile = retry.Profile{
		InitialInterval: 100 * time.Millisecond,
		Multiplier:      1.5,
		MaxInterval:     3 * time.Second,
	}
	// ErrEmptyTxID is returned when a transaction ID query is called with an empty tx_id.
	ErrEmptyTxID = errors.New("tx_id must not be empty")
)

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
	deliveryParams.Metrics = metrics.delivery
	relayService := newRelay(c.LastCommittedBlockSetInterval, metrics)

	return &Service{
		deliveryParams:        deliveryParams,
		relay:                 relayService,
		notifier:              newNotifier(c.ChannelBufferSize, &c.Notification, metrics),
		healthcheck:           serve.DefaultHealthCheckService(),
		config:                c,
		metrics:               metrics,
		committedBlock:        make(chan *common.Block, c.ChannelBufferSize),
		committedBlockWithTxs: make(chan *committedBlockWithTxs, c.ChannelBufferSize),
		statusQueue:           make(chan []*committerpb.TxStatus, c.ChannelBufferSize),
		ready:                 channel.NewReady(),
	}, nil
}

// WaitForReady wait for the service to be ready to be exposed as gRPC service.
// If the context ended before the service is ready, returns false.
func (s *Service) WaitForReady(ctx context.Context) bool {
	return s.ready.WaitForReady(ctx)
}

// Run starts the sidecar service. The call to Run blocks until an error occurs or the context is canceled.
func (s *Service) Run(ctx context.Context) error {
	// Deliver the block with status to client.
	blockStoreInstance, err := newBlockStore(s.config.Ledger.Path, s.config.Ledger.SyncInterval, s.metrics)
	if err != nil {
		return fmt.Errorf("failed to create block store: %w", err)
	}
	defer blockStoreInstance.close()
	s.blockStore = blockStoreInstance
	s.ready.SignalReady()
	defer s.ready.Reset()

	logger.Infof("Create coordinator client and connect to %s", s.config.Committer.Endpoint)
	conn, connErr := connection.NewSingleConnection(s.config.Committer)
	if connErr != nil {
		return errors.Wrapf(connErr, "failed to connect to coordinator")
	}
	s.coordConn = conn
	defer connection.CloseConnectionsLog(conn)
	logger.Infof("sidecar connected to coordinator at %s", s.config.Committer.Endpoint)
	coordClient := servicepb.NewCoordinatorClient(conn)

	pCtx, pCancel := context.WithCancel(ctx)
	defer pCancel()
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
		return s.notifier.run(gCtx, s.statusQueue, s.committedBlockWithTxs)
	})

	g.Go(func() error {
		// TODO: initialize retry from config.
		return retry.Sustain(gCtx, nil, func() error {
			defer func() {
				s.recoverCommittedBlocks(gCtx)
			}()
			return s.sendBlocksAndReceiveStatus(gCtx, coordClient)
		})
	})

	return utils.ProcessErr(g.Wait(), "sidecar has been stopped")
}

// RegisterService registers the sidecar's gRPC services and monitoring server.
func (s *Service) RegisterService(srv serve.Servers) {
	peer.RegisterDeliverServer(srv.GRPC, s)
	committerpb.RegisterBlockQueryServiceServer(srv.GRPC, s)
	committerpb.RegisterNotifierServer(srv.GRPC, s.notifier)
	healthgrpc.RegisterHealthServer(srv.GRPC, s.healthcheck)
	serve.RegisterDynamicTLSUpdater(srv.GrpcTLSProvider, &s.tlsUpdater)
	monitoring.RegisterMonitoringServer(srv.HTTP, s.metrics.Provider)
}

func (s *Service) sendBlocksAndReceiveStatus(
	ctx context.Context,
	coordClient servicepb.CoordinatorClient,
) error {
	defer s.metrics.coordConnection.Disconnected(s.coordConn.CanonicalTarget())
	nextBlockNum, err := s.recover(ctx, coordClient)
	if err != nil {
		return errors.Join(retry.ErrBackOff, err)
	}

	// if the recovery is successful, the connection is established.
	s.metrics.coordConnection.Connected(s.coordConn.CanonicalTarget())

	g, gCtx := errgroup.WithContext(ctx)

	// We drop all enqueued block if any before starting a new session.
	blocksToBeCommitted := make(chan *common.Block, s.config.ChannelBufferSize)
	s.blockToBeCommitted.Store(new(blocksToBeCommitted))

	// Config blocks are infrequent, but in rare cases a user may submit
	// multiple config transactions in rapid succession. Buffer of 5 allows
	// the relay to enqueue without blocking while TLS updater processes.
	configBlocks := make(chan *common.Block, 5)
	// Prime the channel with the latest config block from the store.
	// This ensures TLS is initialized with current CAs before new blocks arrive.
	_, configBlk, err := s.getPrevBlockAndItsConfig(nextBlockNum)
	if err != nil {
		return errors.Wrap(err, "failed to fetch latest config block for TLS initialization")
	}
	if configBlk != nil {
		configBlocks <- configBlk
	}
	g.Go(func() error {
		return s.updateDynamicTLS(gCtx, configBlocks)
	})

	// NOTE: deliver.s.startDelivery and relay.Run must always return an error on exist.
	g.Go(func() error {
		return s.startDelivery(gCtx, blocksToBeCommitted, nextBlockNum)
	})

	g.Go(func() error {
		logger.Info("Relay the blocks to committer (from s.blockToBeCommitted) and receive the transaction status.")
		return s.relay.run(gCtx, &relayRunConfig{
			coordClient:                    coordClient,
			nextExpectedBlockByCoordinator: nextBlockNum,
			incomingBlockToBeCommitted:     blocksToBeCommitted,
			outgoingCommittedBlock:         s.committedBlock,
			outgoingStatusUpdates:          s.statusQueue,
			outgoingConfigBlocks:           configBlocks,
			outgoingCommittedBlockWithTxs:  s.committedBlockWithTxs,
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
	lastBlock, nextBlockVerificationConfig, err := s.getPrevBlockAndItsConfig(nextBlockNum)
	if err != nil {
		return err
	}

	latestConfig := nextBlockVerificationConfig
	blockStoreHeight := s.blockStore.GetBlockHeight()
	if blockStoreHeight > nextBlockNum {
		_, latestConfig, err = s.getPrevBlockAndItsConfig(blockStoreHeight)
		if err != nil {
			return err
		}
	}

	logger.Infof("Staring delivery from block [%d]", nextBlockNum)
	p := s.deliveryParams
	p.OutputBlock = output
	p.LastBlock = lastBlock
	p.NextBlockVerificationConfig = nextBlockVerificationConfig
	p.LatestKnownConfig = deliverorderer.MaxBlock(p.LatestKnownConfig, latestConfig)
	sessionInfo, deliverErr := deliverorderer.ToQueue(ctx, p)

	// The session's latest config block is updated to be the latest config block seen so far.
	// This includes:
	//  - the config block from the YAML (s.deliveryParam),
	//  - the config block from the ledger (latestConfig),
	//  - and the latest config block from the previous delivery runs (session.LatestKnownConfig).
	// This ensures we won't miss a crucial config-block that updated all the endpoints and/or the credentials.
	if sessionInfo != nil {
		s.deliveryParams.LatestKnownConfig = sessionInfo.LatestKnownConfig
	}

	if errors.Is(deliverErr, context.Canceled) {
		// A context may be canceled due to a relay error, thus it is not critical error.
		return errors.Wrap(deliverErr, "context is canceled")
	}
	return errors.Join(retry.ErrNonRetryable, deliverErr)
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
	if configBlockIdx == blk.Header.Number {
		// Config blocks may point to itself.
		return blk, blk, nil
	}
	configBlk, err = s.blockStore.store.RetrieveBlockByNumber(configBlockIdx)
	if err != nil {
		return nil, nil, errors.Wrapf(err, "failed to get config block [%d]", configBlockIdx)
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

		blockToBeCommittedChan := s.blockToBeCommitted.Load()
		if blockToBeCommittedChan != nil {
			promutil.SetGauge(m.yetToBeCommittedBlocksQueueSize, len(*blockToBeCommittedChan))
		}
		promutil.SetGauge(m.committedBlocksQueueSize, len(s.committedBlock))
	}
}

// updateDynamicTLS reads config blocks from the relay and updates the
// dynamic TLS CA certificates. Errors are non-retryable because a failure
// to parse a validated config block indicates a serious problem.
func (s *Service) updateDynamicTLS(ctx context.Context, configBlocks <-chan *common.Block) error {
	reader := channel.NewReader(ctx, configBlocks)
	for ctx.Err() == nil {
		configBlk, ok := reader.Read()
		if !ok {
			return errors.Wrap(ctx.Err(), "context ended")
		}

		if len(configBlk.Data.Data) == 0 {
			return errors.Join(retry.ErrNonRetryable,
				errors.Newf("config block %d has no data", configBlk.Header.Number))
		}

		certs, err := serialization.ExtractAppTLSCAsFromEnvelope(configBlk.Data.Data[0])
		if err != nil {
			return errors.Join(retry.ErrNonRetryable,
				errors.Wrap(err, "failed to extract TLS CAs from config envelope"))
		}

		s.tlsUpdater.UpdateClientRootCAs(certs)

		logger.Infof("Updated dynamic TLS with %d CA certificates", len(certs))
	}

	return errors.Wrap(ctx.Err(), "context cancelled")
}

// ============================================================
// Block Delivery
// ============================================================

// Deliver delivers the requested blocks.
func (s *Service) Deliver(srv peer.Deliver_DeliverServer) error {
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
		retStatus, err := s.deliverBlocks(srv, envelope)
		if err != nil {
			logger.Infof("Failed delivering to %s with status %v: %v", addr, retStatus, err)
			return wrapDeliverError(retStatus, err)
		}
		logger.Infof("Done delivering to %s", addr)

		if err = srv.Send(&peer.DeliverResponse{
			Type: &peer.DeliverResponse_Status{Status: retStatus},
		}); err != nil {
			logger.Infof("Error sending to %s: %s", addr, err)
			return grpcerror.WrapInternalError(err)
		}
	}
}

// DeliverFiltered implements an API in peer.DeliverServer.
//
// Deprecated: this method is implemented to have compatibility with Fabric so that the fabric smart client
// can easily integrate with both FabricX and Fabric. Eventually, this method will be removed.
func (*Service) DeliverFiltered(peer.Deliver_DeliverFilteredServer) error {
	return grpcerror.WrapUnimplemented(errors.New("method is deprecated"))
}

// DeliverWithPrivateData implements an API in peer.DeliverServer.
//
// Deprecated: this method is implemented to have compatibility with Fabric so that the fabric smart client
// can easily integrate with both FabricX and Fabric. Eventually, this method will be removed.
func (*Service) DeliverWithPrivateData(peer.Deliver_DeliverWithPrivateDataServer) error {
	return grpcerror.WrapUnimplemented(errors.New("method is deprecated"))
}

func (s *Service) deliverBlocks(
	srv peer.Deliver_DeliverServer,
	envelope *common.Envelope,
) (common.Status, error) {
	payload, _, err := serialization.ParseEnvelope(envelope)
	if err != nil {
		return common.Status_BAD_REQUEST, errors.Wrap(err, "error parsing envelope")
	}

	seekInfo, err := readSeekInfo(payload.Data)
	if err != nil {
		return common.Status_BAD_REQUEST, err
	}

	ctx := srv.Context()

	if seekInfo.Behavior == ab.SeekInfo_BLOCK_UNTIL_READY {
		// Wait before creating the cursor. FileLedger cannot create a waiting block-zero
		// iterator while the ledger is empty because the block-zero index does not exist yet.
		if !retry.WaitForCondition(ctx, &blockReadyRetryProfile, func() bool {
			return s.blockStore.ledger.Height() > 0
		}) {
			return 0, errors.New("blocks not yet available")
		}
	}

	cursor, stopNum, err := s.getCursor(seekInfo)
	if err != nil {
		return common.Status_BAD_REQUEST, err
	}
	defer cursor.Close()
	logger.Debugf("Received seekInfo.")

	for ctx.Err() == nil {
		block, retStatus := cursor.Next(ctx)
		if retStatus != common.Status_SUCCESS {
			return retStatus, nil
		}

		err = srv.Send(&peer.DeliverResponse{Type: &peer.DeliverResponse_Block{Block: block}})
		if err != nil {
			return common.Status_INTERNAL_SERVER_ERROR, errors.Wrap(err, "error sending response")
		}
		logger.Infof("Successfully sent block %d:%d to client.", block.Header.Number, len(block.Data.Data))

		if stopNum == block.Header.Number {
			break
		}
	}
	return common.Status_SUCCESS, nil
}

func (s *Service) getCursor(seekInfo *ab.SeekInfo) (blockledger.Iterator, uint64, error) {
	cursor, startNum := s.blockStore.ledger.Iterator(seekInfo.Start)

	switch stop := seekInfo.Stop.Type.(type) {
	case *ab.SeekPosition_Oldest:
		return cursor, startNum, nil
	case *ab.SeekPosition_Newest:
		// when seeking only the newest block (i.e. starting
		// and stopping at newest), don't reevaluate the ledger
		// height as this can lead to multiple blocks being
		// sent when only one is expected
		if proto.Equal(seekInfo.Start, seekInfo.Stop) {
			return cursor, startNum, nil
		}
		height := s.blockStore.ledger.Height()
		if height == 0 {
			return cursor, 0, nil
		}
		return cursor, height - 1, nil
	case *ab.SeekPosition_Specified:
		if stop.Specified.Number < startNum {
			cursor.Close()
			return nil, 0, errors.New("start number greater than stop number")
		}
		return cursor, stop.Specified.Number, nil
	default:
		cursor.Close()
		return nil, 0, errors.New("unknown type")
	}
}

// wrapDeliverError wraps deliver errors with appropriate gRPC status codes based on the Fabric status.
func wrapDeliverError(inputStatus common.Status, err error) error {
	if err == nil {
		return nil
	}
	switch inputStatus {
	case common.Status_BAD_REQUEST:
		return grpcerror.WrapInvalidArgument(err)
	case common.Status_NOT_FOUND:
		return grpcerror.WrapNotFound(err)
	default:
		return grpcerror.WrapInternalError(err)
	}
}

func readSeekInfo(payload []byte) (*ab.SeekInfo, error) {
	seekInfo := &ab.SeekInfo{}
	if err := proto.Unmarshal(payload, seekInfo); err != nil {
		return nil, errors.New("malformed seekInfo payload")
	}
	if seekInfo.Start == nil || seekInfo.Stop == nil {
		return nil, errors.New("seekInfo missing start or stop")
	}
	return seekInfo, nil
}

// ============================================================
// Block Query
// ============================================================

// GetBlockchainInfo returns the current blockchain height and hash metadata.
func (s *Service) GetBlockchainInfo(_ context.Context, _ *emptypb.Empty) (*common.BlockchainInfo, error) {
	info, err := s.blockStore.store.GetBlockchainInfo()
	if err != nil {
		logger.Errorf("GetBlockchainInfo failed: %v", err)
		return nil, grpcerror.WrapInternalError(err)
	}
	return info, nil
}

// GetBlockByNumber retrieves a block by its sequence number.
func (s *Service) GetBlockByNumber(_ context.Context, req *committerpb.BlockNumber) (*common.Block, error) {
	block, err := s.blockStore.store.RetrieveBlockByNumber(req.GetNumber())
	if err != nil {
		return nil, wrapQueryError(err)
	}
	return block, nil
}

// GetBlockByTxID retrieves the block that contains the specified transaction.
func (s *Service) GetBlockByTxID(_ context.Context, req *committerpb.TxID) (*common.Block, error) {
	if req.GetTxId() == "" {
		return nil, grpcerror.WrapInvalidArgument(ErrEmptyTxID)
	}
	block, err := s.blockStore.store.RetrieveBlockByTxID(req.GetTxId())
	if err != nil {
		return nil, wrapQueryError(err)
	}
	return block, nil
}

// GetTxByID retrieves the transaction envelope for the specified transaction ID.
func (s *Service) GetTxByID(_ context.Context, req *committerpb.TxID) (*common.Envelope, error) {
	if req.GetTxId() == "" {
		return nil, grpcerror.WrapInvalidArgument(ErrEmptyTxID)
	}
	envelope, err := s.blockStore.store.RetrieveTxByID(req.GetTxId())
	if err != nil {
		return nil, wrapQueryError(err)
	}
	return envelope, nil
}

func wrapQueryError(err error) error {
	if errors.Is(err, blkstorage.ErrNotFound) {
		return grpcerror.WrapNotFound(err)
	}
	logger.Errorf("Unexpected block store error: %v", err)
	return grpcerror.WrapInternalError(err)
}

func waitForIdleCoordinator(ctx context.Context, client servicepb.CoordinatorClient) error {
	for {
		idle, err := client.NoPendingTransactionProcessing(ctx, nil)
		if err != nil {
			return logAndWrapCoordinatorError(err, "failed to check pending transaction processing from coordinator")
		}
		if idle.GetValue() {
			return nil
		}
		logger.Info("Waiting for coordinator to complete processing pending transactions")
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
