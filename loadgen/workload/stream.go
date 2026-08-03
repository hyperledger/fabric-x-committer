/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package workload

import (
	"context"

	"github.com/cockroachdb/errors"
	"golang.org/x/sync/errgroup"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
)

// TxStream yields transactions from the  stream.
type TxStream struct {
	options        *StreamOptions
	gens           []*IndependentTxGenerator
	queue          chan []*servicepb.LoadGenTx
	rateController *ConsumerRateController[*servicepb.LoadGenTx]
}

// NewTxStream creates a stream that generates transactions in batches into a queue.
func NewTxStream(
	profile *Profile,
	options *StreamOptions,
	modifierGenerators ...Generator[Modifier],
) (*TxStream, error) {
	gens, err := newIndependentTxGenerators(profile, modifierGenerators...)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create tx generators")
	}
	queue := make(chan []*servicepb.LoadGenTx, max(options.BuffersSize, 1))
	return &TxStream{
		options:        options,
		queue:          queue,
		gens:           gens,
		rateController: NewConsumerRateController(options.RateLimit, queue),
	}, nil
}

// Run starts the stream workers.
func (s *TxStream) Run(ctx context.Context) error {
	logger.Debugf("Starting %d workers to generate load", len(s.gens))
	g, gCtx := errgroup.WithContext(ctx)
	for _, gen := range s.gens {
		g.Go(func() error {
			return s.generateBatches(gCtx, gen)
		})
	}
	return errors.Wrap(g.Wait(), "stream finished")
}

// generateBatches builds and signs batches from gen and writes them to the stream's queue
// until the context ends.
func (s *TxStream) generateBatches(ctx context.Context, gen *IndependentTxGenerator) error {
	batchSize := max(int(s.options.GenBatch), 1)
	q := channel.NewWriter(ctx, s.queue)
	for q.Write(gen.buildAndSignBatch(batchSize)) {
	}
	return nil
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
