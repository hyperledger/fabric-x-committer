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
) *TxStream {
	queue := make(chan []*servicepb.LoadGenTx, max(options.BuffersSize, 1))
	return &TxStream{
		options:        options,
		queue:          queue,
		gens:           newIndependentTxGenerators(profile, modifierGenerators...),
		rateController: NewConsumerRateController(options.RateLimit, queue),
	}
}

// Run starts the stream workers.
func (s *TxStream) Run(ctx context.Context) error {
	logger.Debugf("Starting %d workers to generate load", len(s.gens))
	g, gCtx := errgroup.WithContext(ctx)
	for _, gen := range s.gens {
		g.Go(func() error {
			ingestBatchesToQueue(gCtx, s.queue, gen, int(s.options.GenBatch))
			return nil
		})
	}
	return errors.Wrap(g.Wait(), "stream finished")
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

func ingestBatchesToQueue[T any](ctx context.Context, c chan<- []T, g Generator[T], batchSize int) {
	batchGen := &MultiGenerator[T]{
		Gen:   g,
		Count: &ConstGenerator[int]{Const: max(batchSize, 1)},
	}
	q := channel.NewWriter(ctx, c)
	for q.Write(batchGen.Next()) {
	}
}
