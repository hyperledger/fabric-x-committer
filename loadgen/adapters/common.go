/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package adapters

import (
	"context"
	"sync/atomic"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-x-common/api/committerpb"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/loadgen/metrics"
	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/logging"
)

type (
	// ClientResources holds client's pre-generated resources to be used by the adapters.
	ClientResources struct {
		Metrics     *metrics.PerfMetrics
		Profile     *workload.Profile
		Stream      *workload.StreamOptions
		ConfigBlock *common.Block
		Limit       *GenerateLimit
	}

	// Phases specify the generation phases to enable.
	Phases struct {
		Config     bool `mapstructure:"config" yaml:"config"`
		Namespaces bool `mapstructure:"namespaces" yaml:"namespaces"`
		Load       bool `mapstructure:"load" yaml:"load"`
	}

	// GenerateLimit describes a stopping condition for generating load according to the collected metrics.
	// Zero value indicate no limit.
	// The limit on the number of TXs is applied at block granularity. I.e., more TXs might be created than expected
	// if a block overshot.
	// The load generator stops when both requirements are met, i.e., one of them might overshoot.
	// For the orderer adapter, the blocks limit is ignored for broadcasting as we don't track submitted blocks.
	// For adapters that use concurrent submitters, we cannot enforce exact limits.
	// The sidecar and coordinator adapters are sequential, so they don't have these issues.
	GenerateLimit struct {
		Blocks       uint64 `mapstructure:"blocks" yaml:"blocks"`
		Transactions uint64 `mapstructure:"transactions" yaml:"transactions"`
	}

	// commonAdapter is used as a base class for the adapters.
	commonAdapter struct {
		res          *ClientResources
		nextBlockNum atomic.Uint64
	}

	// txsWithMapping contains the TXs with its mapping to the adapter's form.
	txsWithMapping[T any] struct {
		mapping T
		txIDs   []string
	}
)

var (
	logger = logging.New("load-gen-adapters")

	// ErrInvalidAdapterConfig indicates that none of the adapter's config were given.
	ErrInvalidAdapterConfig = errors.New("invalid config passed")
)

// PhasesIntersect returns the phases that are both in p1 and p2.
// If one of them is empty, it is assumed not to be specified.
func PhasesIntersect(p1, p2 Phases) Phases {
	if p1.Empty() {
		return p2
	}
	if p2.Empty() {
		return p1
	}
	return Phases{
		Config:     p1.Config && p2.Config,
		Namespaces: p1.Namespaces && p2.Namespaces,
		Load:       p1.Load && p2.Load,
	}
}

// Empty returns true if no phase was applied.
func (p *Phases) Empty() bool {
	return p == nil || (!p.Config && !p.Namespaces && !p.Load)
}

// Progress a committed transaction indicate progress for most adapters.
func (c *commonAdapter) Progress() uint64 {
	return c.res.Metrics.GetState().TransactionsCommitted
}

// Supports specify which phases an adapter supports.
func (*commonAdapter) Supports() Phases {
	return Phases{
		Config:     true,
		Namespaces: true,
		Load:       true,
	}
}

// NextBlockNum returns the next block number to use.
func (c *commonAdapter) NextBlockNum() uint64 {
	return c.nextBlockNum.Add(1) - 1
}

// sendBlocks submits TXs from the stream using the sender.
// It uses the mapper to map the TX stream to the sender's form.
// The mapper also outputs a list of TX IDs that are used to track the TX latency.
//
//nolint:revive // Parameters are required.
func sendBlocks[T any](
	ctx context.Context,
	c *commonAdapter,
	txStream *workload.StreamWithSetup,
	mapper func(uint64, []*servicepb.LoadGenTx) T,
	sender func(T) error,
) error {
	queueRaw := make(chan *txsWithMapping[T], c.res.Stream.BuffersSize)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Pipeline the mapping process.
	go func() {
		defer close(queueRaw)
		mapToQueue(ctx, c, queueRaw, txStream, mapper, c.res.Profile.Block)
	}()

	queue := channel.NewReader(ctx, queueRaw)
	for ctx.Err() == nil {
		b, ok := queue.Read()
		if !ok {
			// The context ended.
			return nil
		}
		logger.Debugf("Sending block with %d TXs", len(b.txIDs))
		if err := sender(b.mapping); err != nil {
			return errors.Wrap(connection.FilterStreamRPCError(err), "failed sending block")
		}
		c.res.Metrics.OnSendBatch(b.txIDs)
		if c.res.isSendLimit() {
			return nil
		}
	}
	return nil
}

//nolint:revive // Parameters are required.
func mapToQueue[T any](
	ctx context.Context,
	c *commonAdapter,
	queueRaw chan *txsWithMapping[T],
	txStream *workload.StreamWithSetup,
	mapper func(uint64, []*servicepb.LoadGenTx) T,
	blockProfile workload.BlockProfile,
) {
	queue := channel.NewWriter(ctx, queueRaw)
	txGen := txStream.MakeTxGenerator()
	for ctx.Err() == nil {
		txs := txGen.Next(ctx, blockProfile)
		if len(txs) == 0 || txs[len(txs)-1] == nil {
			// Generators return nil when their stream is done.
			// This indicates that the block generator should also be done.
			return
		}
		queue.Write(&txsWithMapping[T]{
			mapping: mapper(c.NextBlockNum(), txs),
			txIDs:   getTXsIDs(txs),
		})
	}
}

func getTXsIDs(txs []*servicepb.LoadGenTx) []string {
	txIDs := make([]string, len(txs))
	for i, tx := range txs {
		txIDs[i] = tx.Id
	}
	return txIDs
}

func toMetricsStatus(txStatus []*committerpb.TxStatus) []metrics.TxStatus {
	statusBatch := make([]metrics.TxStatus, len(txStatus))
	for i, status := range txStatus {
		statusBatch[i] = metrics.TxStatus{TxID: status.Ref.TxId, Status: status.Status}
	}
	return statusBatch
}

func (r *ClientResources) isSendLimit() bool {
	if !r.Limit.HasLimit() {
		return false
	}
	state := r.Metrics.GetState()
	return isReachedLimit(state.BlocksSent, r.Limit.Blocks) &&
		isReachedLimit(state.TransactionsSent, r.Limit.Transactions)
}

func (r *ClientResources) isTXSendLimit() bool {
	if r.Limit == nil || r.Limit.Transactions == 0 {
		return false
	}
	state := r.Metrics.GetState()
	return isReachedLimit(state.TransactionsSent, r.Limit.Transactions)
}

func (r *ClientResources) isReceiveLimit() bool {
	if !r.Limit.HasLimit() {
		return false
	}
	state := r.Metrics.GetState()
	return isReachedLimit(state.BlocksReceived, r.Limit.Blocks) &&
		isReachedLimit(state.TransactionsReceived, r.Limit.Transactions)
}

// HasLimit returns true if any limit is set.
func (g *GenerateLimit) HasLimit() bool {
	return g != nil && (g.Blocks > 0 || g.Transactions > 0)
}

func isReachedLimit(value, limit uint64) bool {
	return limit == 0 || value >= limit
}
