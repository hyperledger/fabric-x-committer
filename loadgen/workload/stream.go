/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package workload

import (
	"context"
	"math/rand"

	"github.com/cockroachdb/errors"
	"golang.org/x/sync/errgroup"
	"golang.org/x/time/rate"

	"github.com/hyperledger/fabric-x-committer/api/protoloadgen"
	"github.com/hyperledger/fabric-x-committer/api/protoqueryservice"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
)

type (
	// TxStream yields transactions from the  stream.
	TxStream struct {
		stream
		gens  []*IndependentTxGenerator
		queue chan []*protoloadgen.TX
	}

	// QueryStream generates stream's queries consumers.
	QueryStream struct {
		stream
		gen   []*QueryGenerator
		queue chan []*protoqueryservice.Query
	}

	stream struct {
		options *StreamOptions
		limiter *rate.Limiter
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
	txStream := &TxStream{
		stream: newStream(profile, options),
		queue:  make(chan []*protoloadgen.TX, max(options.BuffersSize, 1)),
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
		txGen := newIndependentTxGenerator(txGenSeed, w.keyGen, &profile.Transaction, modifiers...)
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
func (s *TxStream) AppendBatch(ctx context.Context, batch []*protoloadgen.TX) {
	channel.NewWriter(ctx, s.queue).Write(batch)
}

// GetLimit reads the stream limit.
func (s *TxStream) GetLimit() rate.Limit {
	return s.limiter.Limit()
}

// SetLimit sets the stream limit.
func (s *TxStream) SetLimit(limit rate.Limit) {
	s.limiter.SetLimit(limit)
}

// MakeGenerator creates a new generator that consumes from the stream.
// Each generator must be used from a single goroutine, but different
// generators from the same Stream can be used concurrently.
func (s *TxStream) MakeGenerator() *RateLimiterGenerator[*protoloadgen.TX] {
	return NewRateLimiterGenerator(s.queue, s.limiter)
}

// NewQueryGenerator creates workers that generates queries into a queue.
func NewQueryGenerator(profile *Profile, options *StreamOptions) *QueryStream {
	qs := &QueryStream{
		stream: newStream(profile, options),
		queue:  make(chan []*protoqueryservice.Query, max(options.BuffersSize, 1)),
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
func (s *QueryStream) MakeGenerator() *RateLimiterGenerator[*protoqueryservice.Query] {
	return NewRateLimiterGenerator(s.queue, s.limiter)
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

func newStream(profile *Profile, options *StreamOptions) stream {
	// We allow bursting with a full block.
	// We also need to support bursting with the size of the generated batch
	// as we fetch the entire batch regardless of the block size.
	burst := max(int(options.GenBatch), int(profile.Block.Size)) //nolint:gosec // uint64 -> int.
	return stream{
		options: options,
		limiter: NewLimiter(options.RateLimit, burst),
	}
}
