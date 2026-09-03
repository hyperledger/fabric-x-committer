/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package workload

import (
	"context"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

type (
	// TxStream yields transactions from the  stream.
	TxStream struct {
		options        *StreamOptions
		gens           []*IndependentTxGenerator
		queue          chan []*servicepb.LoadGenTx
		rateController *ConsumerRateController[*servicepb.LoadGenTx]
	}

	// batchQuerier optionally fills committed key versions into a batch's reads before signing. It is the
	// one pluggable seam in the build -> query -> sign pipeline: a real querier needs infrastructure (a
	// query-service client) the workload package should not otherwise depend on, and it is wired PER WORKER
	// — a gRPC ClientConn is safe to share, but a per-worker connection gives independent HTTP/2 flow
	// control and stream-concurrency headroom under load. A nil querier means no query stage: reads keep
	// their nil versions, so the batch is identical to one that was only built and signed.
	//
	// Why FillVersions takes NEITHER the transaction index NOR a deterministic per-TX query count: the
	// workload determinism contract covers only the generated transaction DATA — keys, values, metadata,
	// and nonce/TX-ID — as a pure function of the tx index. It does NOT cover execution OUTCOME: whether a
	// TX commits or aborts, and whether a versioned read is still current at commit time, both depend on
	// the non-deterministic commit order (parallel submission, out-of-order commit). So a deterministic
	// per-TX query count would not make per-TX success deterministic — there is nothing to gain from tying
	// the query decision to the tx index. Instead queries-rate is an AVERAGE number of reads versioned per
	// TX that a concrete querier realizes with its own rate mechanism (e.g. a stateful accumulator), NOT a
	// cumulative floor over the tx index. Within each TX it versions the highest-priority reads first by
	// iterating the read set in REVERSE: reads are laid out new-keys-first, so back-references (existing,
	// committed keys) come before newly-introduced ones.
	batchQuerier interface {
		// FillVersions fills in the committed version of the reads (ReadsOnly and ReadWrites) it selects
		// across the whole batch of unsigned transactions, in place, before the batch is signed.
		FillVersions(ctx context.Context, batch []*applicationpb.Tx) error
	}
)

// NewTxStream creates a stream that generates transactions in batches into a queue. The counter is the
// shared transaction-index counter (created before the stream and shared with the metrics); the stream's
// workers reserve their index ranges from it via the generators.
func NewTxStream(profile *Profile, options *StreamOptions, counter *TxCounter) (*TxStream, error) {
	gens, err := newIndependentTxGenerators(profile, counter)
	if err != nil {
		return nil, err
	}
	queue := make(chan []*servicepb.LoadGenTx, max(options.BuffersSize, 1))
	return &TxStream{
		options:        options,
		queue:          queue,
		gens:           gens,
		rateController: NewConsumerRateController(options.RateLimit, queue),
	}, nil
}

// Run starts the stream workers. When queryClient enables the query stage (set and queries-rate > 0) Run
// dials one load-balanced query-service connection per worker and each worker versions a rate-controlled
// subset of its batches' reads before signing; otherwise every worker runs without a querier. The dialed
// connections are closed when Run returns, coupling their lifetime to the run.
func (s *TxStream) Run(ctx context.Context, queryClient *connection.MultiClientConfig) error {
	logger.Debugf("Starting %d workers to generate load", len(s.gens))
	queriers, conns, err := s.dialQueryConnections(queryClient)
	if err != nil {
		return err
	}
	defer connection.CloseConnectionsLog(conns...)

	g, gCtx := errgroup.WithContext(ctx)
	for i, gen := range s.gens {
		g.Go(func() error {
			return s.generateBatches(gCtx, gen, queriers[i])
		})
	}
	return errors.Wrap(g.Wait(), "stream finished")
}

// dialQueryConnections builds one query filler per worker — each backed by its own load-balanced
// query-service connection — when the query stage is enabled, and returns an all-nil filler slice (no
// versioning for any worker) when it is disabled. It hands back both the fillers the workers use and the
// connections whose lifetime Run owns (Run closes them when it returns). The stage is enabled when the
// workers version reads — queries-rate > 0, uniform across workers, so gens[0] speaks for all. It then
// requires a query-client config: config-load validation already guarantees one, so a nil here is a caller
// bug and Run fails loudly rather than silently regressing versioned reads to nil-version creates. Each
// connection round-robins across the configured endpoints. On a dial error it closes any connection
// already opened before returning.
func (s *TxStream) dialQueryConnections(
	queryClient *connection.MultiClientConfig,
) ([]batchQuerier, []*grpc.ClientConn, error) {
	queriers := make([]batchQuerier, len(s.gens))
	if len(s.gens) == 0 || s.gens[0].QueriesRate <= 0 {
		return queriers, nil, nil
	}
	if queryClient == nil {
		return nil, nil, errors.New("query stage enabled (queries-rate > 0) but no query-client configured")
	}
	conns := make([]*grpc.ClientConn, 0, len(s.gens))
	for i := range s.gens {
		conn, err := connection.NewLoadBalancedConnection(queryClient)
		if err != nil {
			connection.CloseConnectionsLog(conns...)
			return nil, nil, errors.Wrap(err, "failed to connect to query service")
		}
		conns = append(conns, conn)
		queriers[i] = newQueryFiller(committerpb.NewQueryServiceClient(conn), s.gens[i].QueriesRate)
	}
	return queriers, conns, nil
}

// AppendBatch appends a batch to the stream.
func (s *TxStream) AppendBatch(ctx context.Context, batch []*servicepb.LoadGenTx) {
	channel.NewWriter(ctx, s.queue).Write(batch)
}

// GetRate reads the stream limit.
func (s *TxStream) GetRate() uint64 {
	return s.rateController.Rate()
}

// SetRate sets the stream limit.
func (s *TxStream) SetRate(rate uint64) {
	s.rateController.SetRate(rate)
}

// MakeGenerator creates a new generator that consumes from the stream.
// Each generator must be used from a single goroutine, but different
// generators from the same Stream can be used concurrently.
func (s *TxStream) MakeGenerator() *ConsumerRateController[*servicepb.LoadGenTx] {
	return s.rateController.InstantiateWorker()
}

// generateBatches builds, optionally versions, signs, and enqueues batches in a loop until the context
// ends (or the querier errors). Generation (buildBatch/signBatch) is context-free; only the optional
// query stage does I/O, so the context lives here in the worker, not in the generator.
func (s *TxStream) generateBatches(ctx context.Context, gen *IndependentTxGenerator, querier batchQuerier) error {
	batchSize := max(uint64(s.options.GenBatch), 1)
	q := channel.NewWriter(ctx, s.queue)
	for {
		txs, base := gen.buildBatch(batchSize)
		if querier != nil {
			if err := querier.FillVersions(ctx, txs); err != nil {
				if ctx.Err() != nil {
					// Context ended: clean shutdown, same as an interrupted queue write below — the
					// in-flight query was cancelled by teardown, not a query failure.
					return nil //nolint:nilerr // context cancellation is clean shutdown, not an error.
				}
				return err
			}
		}
		if !q.Write(gen.signBatch(txs, base)) {
			return nil
		}
	}
}
