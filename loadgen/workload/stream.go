/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package workload

import (
	"context"
	"math/rand"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"golang.org/x/sync/errgroup"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
)

type (
	// TxStream yields transactions from the  stream.
	TxStream struct {
		options        *StreamOptions
		gens           []*IndependentTxGenerator
		queue          chan []*servicepb.LoadGenTx
		rateController *ConsumerRateController[*servicepb.LoadGenTx]
	}

	// QueryStream generates stream's queries consumers.
	QueryStream struct {
		options        *StreamOptions
		gen            []*QueryGenerator
		queue          chan []*committerpb.Query
		rateController *ConsumerRateController[*committerpb.Query]
	}
)

// NewTxStream creates workers that generates transactions into a queue and apply the modifiers.
// Each worker will have a unique instance of the modifier to avoid concurrency issues.
// The modifiers will be applied in the order they are given.
// A transaction modifier can modify any of its fields to adjust the workload.
// For example, a modifier can query the database for the read-set versions to simulate a real transaction.
// The signature modifier is applied last so all previous modifications will be signed correctly.
func NewTxStream(
	profile *Profile,
	options *StreamOptions,
	modifierGenerators ...Generator[Modifier],
) *TxStream {
	queue := make(chan []*servicepb.LoadGenTx, max(options.BuffersSize, 1))
	txStream := &TxStream{
		options:        options,
		queue:          queue,
		rateController: NewConsumerRateController(options.RateLimit, queue),
	}
	for _, w := range makeWorkersData(profile) {
		modifiers := make([]Modifier, 0, len(modifierGenerators)+2)
		if len(profile.Conflicts.Dependencies) > 0 {
			modifiers = append(modifiers, newTxDependenciesModifier(NewRandFromSeedGenerator(w.seed), profile))
		}
		for _, mod := range modifierGenerators {
			modifiers = append(modifiers, mod.Next())
		}
		modifiers = append(modifiers, newSignTxModifier(NewRandFromSeedGenerator(w.seed), profile))

		txGenSeed := NewRandFromSeedGenerator(w.seed)
		txGen := newIndependentTxGenerator(txGenSeed, w.keyGen, &profile.Transaction, &profile.Policy, modifiers...)
		txStream.gens = append(txStream.gens, txGen)
	}
	return txStream
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

// NewQueryGenerator creates workers that generates queries into a queue.
func NewQueryGenerator(profile *Profile, options *StreamOptions) *QueryStream {
	queue := make(chan []*committerpb.Query, max(options.BuffersSize, 1))
	qs := &QueryStream{
		options:        options,
		queue:          queue,
		rateController: NewConsumerRateController(options.RateLimit, queue),
	}
	for _, w := range makeWorkersData(profile) {
		queryGen := newQueryGenerator(NewRandFromSeedGenerator(w.seed), w.keyGen, profile)
		qs.gen = append(qs.gen, queryGen)
	}
	return qs
}

// Run starts the workers.
func (s *QueryStream) Run(ctx context.Context) error {
	logger.Debugf("Starting %d workers to generate query load", len(s.gen))

	g, gCtx := errgroup.WithContext(ctx)
	for _, gen := range s.gen {
		g.Go(func() error {
			ingestBatchesToQueue(gCtx, s.queue, gen, int(s.options.GenBatch))
			return nil
		})
	}
	return errors.Wrap(g.Wait(), "stream finished")
}

// MakeGenerator creates a new generator that consumes from the stream.
// Each generator must be used from a single goroutine, but different
// generators from the same Stream can be used concurrently.
func (s *QueryStream) MakeGenerator() *ConsumerRateController[*committerpb.Query] {
	return s.rateController.InstantiateWorker()
}

type workerData struct {
	seed   *rand.Rand
	keyGen *ByteArrayGenerator
}

func makeWorkersData(profile *Profile) []workerData {
	seedGen := rand.New(rand.NewSource(profile.Seed))
	workers := make([]workerData, profile.Workers)
	for i := range workers {
		seed := NewRandFromSeedGenerator(seedGen)
		// Each worker has a unique seed to generate keys in addition the seed for the other content.
		// This allows reproducing the generated keys regardless of the other generated content.
		// It is useful when generating transactions, and later generating queries for the same keys.
		keySeed := NewRandFromSeedGenerator(seed)
		workers[i] = workerData{
			seed:   seed,
			keyGen: &ByteArrayGenerator{Size: profile.Key.Size, Source: keySeed},
		}
	}
	return workers
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
