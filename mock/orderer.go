/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package mock

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	ab "github.com/hyperledger/fabric-protos-go-apiv2/orderer"
	"github.com/hyperledger/fabric-x-common/common/util"
	"github.com/hyperledger/fabric-x-common/protoutil"
	"github.com/hyperledger/fabric-x-common/tools/configtxgen"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/grpcerror"
)

type (
	// OrdererConfig configuration for the mock orderer.
	OrdererConfig struct {
		// Server and ServerConfigs sets the used serving endpoints.
		// We support both for compatibility with other services.
		Server           *connection.ServerConfig   `mapstructure:"server"`
		ServerConfigs    []*connection.ServerConfig `mapstructure:"servers"`
		BlockSize        int                        `mapstructure:"block-size"`
		BlockTimeout     time.Duration              `mapstructure:"block-timeout"`
		OutBlockCapacity int                        `mapstructure:"out-block-capacity"`
		PayloadCacheSize int                        `mapstructure:"payload-cache-size"`
		ConfigBlockPath  string                     `mapstructure:"config-block-path"`
		SendConfigBlock  bool                       `mapstructure:"send-config-block"`

		// These fields are only used for internal testing.
		NumService int
		TLS        connection.TLSConfig
	}

	// Orderer supports running multiple mock-orderer services which mocks a consortium.
	Orderer struct {
		config      *OrdererConfig
		configBlock *common.Block
		inEnvs      chan *common.Envelope
		inBlocks    chan *common.Block
		cutBlock    chan any
		cache       *blockCache
		healthcheck *health.Server
	}

	// HoldingOrderer allows holding a block.
	HoldingOrderer struct {
		*Orderer
		HoldFromBlock atomic.Uint64
	}

	holdingStream struct {
		ab.AtomicBroadcast_DeliverServer
		holdBlock *atomic.Uint64
	}

	blockCache struct {
		storage           []*common.Block
		maxDeliveredBlock int64
		mu                *sync.Cond
	}
)

var (
	// ErrLostBlock is returned if a block was removed from the cache.
	ErrLostBlock = errors.New("lost block")

	defaultConfig = OrdererConfig{
		NumService:       1,
		BlockSize:        100,
		BlockTimeout:     100 * time.Millisecond,
		OutBlockCapacity: 1024,
		PayloadCacheSize: 1024,
	}
	defaultConfigBlock = &common.Block{
		Header: &common.BlockHeader{
			Number: 0,
		},
		Data: &common.BlockData{Data: [][]byte{protoutil.MarshalOrPanic(&common.Envelope{
			Payload: protoutil.MarshalOrPanic(&common.Payload{
				Header: &common.Header{
					ChannelHeader: protoutil.MarshalOrPanic(&common.ChannelHeader{
						Type: int32(common.HeaderType_CONFIG),
					}),
				},
			}),
		})}},
	}
	repsSuccess = ab.BroadcastResponse{
		Status: common.Status_SUCCESS,
	}
)

// NewMockOrderer creates multiple orderer instances.
func NewMockOrderer(config *OrdererConfig) (*Orderer, error) {
	if config.BlockSize == 0 {
		config.BlockSize = defaultConfig.BlockSize
	}
	if config.BlockTimeout.Abs() == 0 {
		config.BlockTimeout = defaultConfig.BlockTimeout
	}
	if len(config.ServerConfigs) > 0 {
		config.NumService = len(config.ServerConfigs)
	}
	if config.NumService == 0 {
		config.NumService = defaultConfig.NumService
	}
	if config.OutBlockCapacity == 0 {
		config.OutBlockCapacity = defaultConfig.OutBlockCapacity
	}
	if config.PayloadCacheSize == 0 {
		config.PayloadCacheSize = defaultConfig.PayloadCacheSize
	}
	configBlock := defaultConfigBlock
	if config.ConfigBlockPath != "" {
		var err error
		configBlock, err = configtxgen.ReadBlock(config.ConfigBlockPath)
		if err != nil {
			return nil, errors.Wrap(err, "failed to read config block")
		}
	}

	return &Orderer{
		config:      config,
		configBlock: configBlock,
		inEnvs:      make(chan *common.Envelope, config.NumService*config.BlockSize*config.OutBlockCapacity),
		inBlocks:    make(chan *common.Block, config.BlockSize*config.OutBlockCapacity),
		cutBlock:    make(chan any),
		cache:       newBlockCache(config.OutBlockCapacity),
		healthcheck: connection.DefaultHealthCheckService(),
	}, nil
}

// Broadcast receives TXs and returns ACKs.
func (o *Orderer) Broadcast(stream ab.AtomicBroadcast_BroadcastServer) error {
	addr := util.ExtractRemoteAddress(stream.Context())
	logger.Infof("Starting broadcast with %s", addr)
	defer logger.Infof("Finished broadcast with %s", addr)

	inEnvs := channel.NewWriter(stream.Context(), o.inEnvs)
	for {
		env, err := stream.Recv()
		if err != nil {
			return err //nolint:wrapcheck // already a GRPC error.
		}
		inEnvs.Write(env)
		if err = stream.Send(&repsSuccess); err != nil {
			return err //nolint:wrapcheck // already a GRPC error.
		}
	}
}

// Deliver receives a seek request and returns a stream of the orderered blocks.
func (o *Orderer) Deliver(stream ab.AtomicBroadcast_DeliverServer) error {
	env, streamErr := stream.Recv()
	if streamErr != nil {
		return streamErr //nolint:wrapcheck // already a GRPC error.
	}
	start, end, seekErr := readSeekInfo(env)
	if seekErr != nil {
		return grpcerror.WrapInvalidArgument(seekErr)
	}

	addr := util.ExtractRemoteAddress(stream.Context())
	logger.Infof("Starting delivery with %s [%d -> %d]", addr, start, end)
	defer logger.Infof("Finished delivery with %s", addr)

	ctx := stream.Context()
	// Ensures releasing the waiters when this context ends.
	stop := o.cache.releaseAfter(ctx)
	defer stop()

	var prevBlock *common.Block
	for i := start; i <= end && ctx.Err() == nil; i++ {
		b, err := o.cache.getBlock(ctx, i)
		if errors.Is(err, ErrLostBlock) {
			// We send an empty block for the sake of the delivery progress.
			b = &common.Block{Data: &common.BlockData{}}
			addBlockHeader(prevBlock, b)
		} else if err != nil {
			return grpcerror.WrapCancelled(err)
		}
		if err = stream.Send(&ab.DeliverResponse{Type: &ab.DeliverResponse_Block{Block: b}}); err != nil {
			return err //nolint:wrapcheck // already a GRPC error.
		}
		logger.Debugf("Emitted block %d", b.Header.Number)
		prevBlock = b
	}
	return grpcerror.WrapCancelled(ctx.Err())
}

// Deliver calls Orderer.Deliver, but with a holding stream.
func (o *HoldingOrderer) Deliver(stream ab.AtomicBroadcast_DeliverServer) error {
	return o.Orderer.Deliver(&holdingStream{
		AtomicBroadcast_DeliverServer: stream,
		holdBlock:                     &o.HoldFromBlock,
	})
}

// Release blocks.
func (o *HoldingOrderer) Release() {
	o.HoldFromBlock.Store(math.MaxUint64)
}

// RegisterService registers for the orderer's GRPC services.
func (o *HoldingOrderer) RegisterService(server *grpc.Server) {
	ab.RegisterAtomicBroadcastServer(server, o)
	healthgrpc.RegisterHealthServer(server, o.healthcheck)
}

func (s *holdingStream) Send(msg *ab.DeliverResponse) error {
	block, ok := msg.Type.(*ab.DeliverResponse_Block)
	if ok {
		// A simple busy wait loop is sufficient here.
		for block.Block.Header.Number >= s.holdBlock.Load() {
			logger.Infof("Holding block: %d (up to %d)", block.Block.Header.Number, s.holdBlock.Load())
			select {
			case <-s.Context().Done():
				return nil
			case <-time.NewTimer(time.Second).C:
			}
		}
	}
	//nolint:wrapcheck // This is a mock for an external method.
	return s.AtomicBroadcast_DeliverServer.Send(msg)
}

func readSeekInfo(env *common.Envelope) (start, end uint64, err error) {
	start = 0
	end = math.MaxUint64

	payload, err := protoutil.UnmarshalPayload(env.Payload)
	if err != nil {
		return start, end, errors.Wrap(err, "failed unmarshalling payload")
	}
	seekInfo := &ab.SeekInfo{}
	if err = proto.Unmarshal(payload.Data, seekInfo); err != nil {
		return start, end, errors.Wrap(err, "failed unmarshalling seek info")
	}

	if startMsg := seekInfo.Start.GetSpecified(); startMsg != nil {
		start = startMsg.Number
	}
	if endMsg := seekInfo.Stop.GetSpecified(); endMsg != nil {
		end = endMsg.Number
	}
	if start > end {
		return start, end, errors.Newf("invalid block range: (start) [%d] > [%d] (end)", start, end)
	}
	return start, end, nil
}

// Run collects the envelopes, cuts the blocks, and store them to the block cache.
func (o *Orderer) Run(ctx context.Context) error {
	// Ensures releasing the waiters when this context ends.
	stop := o.cache.releaseAfter(ctx)
	defer stop()

	tick := time.NewTicker(o.config.BlockTimeout)
	var prevBlock *common.Block
	sendBlock := func(b *common.Block) {
		if b == nil {
			return
		}
		addBlockHeader(prevBlock, b)
		o.cache.addBlock(ctx, b)
		tick.Reset(o.config.BlockTimeout)
		prevBlock = b
	}

	// Submit the config block.
	if o.config.SendConfigBlock {
		sendBlock(o.configBlock)
	}

	data := make([][]byte, 0, o.config.BlockSize)
	sendBlockData := func(reason string) {
		if len(data) == 0 {
			return
		}
		logger.Debugf("block with [%d] txs has been cut (%s)", len(data), reason)
		sendBlock(&common.Block{Data: &common.BlockData{Data: data}})
		data = make([][]byte, 0, o.config.BlockSize)
	}
	envCache := newFifoCache[any](o.config.PayloadCacheSize)
	for {
		select {
		case <-ctx.Done():
			return errors.Wrapf(ctx.Err(), "context ended")
		case b := <-o.inBlocks:
			sendBlock(b)
		case <-o.cutBlock:
			sendBlockData("external")
		case <-tick.C:
			sendBlockData("timeout")
		case env := <-o.inEnvs:
			if !addEnvelope(envCache, env) {
				continue
			}
			data = append(data, protoutil.MarshalOrPanic(env))
			if len(data) >= o.config.BlockSize {
				sendBlockData("size")
			}
		}
	}
}

// WaitForReady returns true when we are ready to process GRPC requests.
func (*Orderer) WaitForReady(context.Context) bool {
	return true
}

// RegisterService registers for the orderer's GRPC services.
func (o *Orderer) RegisterService(server *grpc.Server) {
	ab.RegisterAtomicBroadcastServer(server, o)
	healthgrpc.RegisterHealthServer(server, o.healthcheck)
}

// SubmitBlock allows submitting blocks directly for testing other packages.
// The block header will be replaced with a generated header.
func (o *Orderer) SubmitBlock(ctx context.Context, b *common.Block) error {
	if !channel.NewWriter(ctx, o.inBlocks).Write(b) {
		return errors.Wrapf(ctx.Err(), "failed to submit block")
	}
	return nil
}

// SubmitEnv allows submitting envelops directly for testing other packages.
func (o *Orderer) SubmitEnv(ctx context.Context, e *common.Envelope) bool {
	return channel.NewWriter(ctx, o.inEnvs).Write(e)
}

// GetBlock allows fetching blocks directly for testing other packages.
func (o *Orderer) GetBlock(ctx context.Context, blockNum uint64) (*common.Block, error) {
	// Ensures releasing the waiters when this context ends.
	stop := o.cache.releaseAfter(ctx)
	defer stop()
	return o.cache.getBlock(ctx, blockNum)
}

// CutBlock allows forcing block cut for testing other packages.
func (o *Orderer) CutBlock(ctx context.Context) bool {
	return channel.NewWriter(ctx, o.cutBlock).Write(nil)
}

func addBlockHeader(prevBlock, curBlock *common.Block) {
	var blockNumber uint64
	var previousHash []byte
	if prevBlock != nil {
		blockNumber = prevBlock.Header.Number + 1
		previousHash = protoutil.BlockHeaderHash(prevBlock.Header)
	}
	curBlock.Header = &common.BlockHeader{
		Number:       blockNumber,
		DataHash:     protoutil.ComputeBlockDataHash(curBlock.Data),
		PreviousHash: previousHash,
	}
}

func newBlockCache(size int) *blockCache {
	return &blockCache{
		storage:           make([]*common.Block, size),
		maxDeliveredBlock: -1,
		mu:                sync.NewCond(&sync.Mutex{}),
	}
}

func (c *blockCache) releaseAfter(ctx context.Context) (stop func() bool) {
	return context.AfterFunc(ctx, func() {
		c.mu.L.Lock()
		defer c.mu.L.Unlock()
		c.mu.Broadcast()
	})
}

// addBlock returns true if the block was inserted.
// It may fail if the context ends.
func (c *blockCache) addBlock(ctx context.Context, b *common.Block) bool {
	blockIndex := int(b.Header.Number) % len(c.storage) //nolint:gosec // integer overflow conversion uint64 -> int

	c.mu.L.Lock()
	defer c.mu.L.Unlock()
	// We ensure that we don't overwrite a block that haven't been delivered yet.
	//nolint:gosec // integer overflow conversion uint64 -> int64
	for c.storage[blockIndex] != nil && int64(c.storage[blockIndex].Header.Number) > c.maxDeliveredBlock {
		if ctx.Err() != nil {
			return false
		}
		c.mu.Wait()
	}
	c.storage[blockIndex] = b
	// Wakeup all waiting to get blocks.
	c.mu.Broadcast()
	return true
}

func (c *blockCache) getBlock(ctx context.Context, blockNum uint64) (*common.Block, error) {
	blockIndex := int(blockNum) % len(c.storage) //nolint:gosec // integer overflow conversion uint64 -> int

	c.mu.L.Lock()
	defer c.mu.L.Unlock()
	for ctx.Err() == nil {
		b := c.storage[blockIndex]
		switch {
		case b == nil || b.Header.Number < blockNum:
			// There is still hope for this block.
			c.mu.Wait()
		case b.Header.Number > blockNum:
			// We'll never see this block again.
			return nil, ErrLostBlock
		default: // b.Header.Number == blockNum
			//nolint:gosec // integer overflow conversion uint64 -> int64
			c.maxDeliveredBlock = max(c.maxDeliveredBlock, int64(b.Header.Number))
			// Wakeup all waiting to add blocks.
			c.mu.Broadcast()
			return b, nil
		}
	}
	return nil, errors.Wrapf(ctx.Err(), "context ended")
}

// addEnvelope returns true if the entry is unique.
// It employs a FIFO eviction policy.
func addEnvelope(c *fifoCache[any], e *common.Envelope) bool {
	if e == nil {
		return false
	}
	digestRaw := sha256.Sum256(e.Payload)
	digest := base64.StdEncoding.EncodeToString(digestRaw[:])
	return c.addIfNotExist(digest, nil)
}
