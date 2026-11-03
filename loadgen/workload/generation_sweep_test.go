/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package workload

import (
	"strconv"
	"testing"

	"github.com/hyperledger/fabric-lib-go/common/flogging"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/utils/signature"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

// The generation rate a deployment can offer bounds what its committer can be measured at, so
// these sweeps exist to find that rate and attribute it. They differ from BenchmarkGenTx in
// three ways that matter on a real deployment: they cover EDDSA, which a deployment picks
// precisely because it is the cheapest scheme to sign; they use a deployment's block size
// rather than the 1024 that keeps the unit benchmarks quick; and they vary one knob at a time
// so a number can be attributed to it.
//
// Run them on the machine that will host the load generator, not a workstation: the answer is
// core-count bound and every scheme below scales with cores until the worker count reaches
// them.
const (
	sweepBlockSize   = 10_000
	sweepBuffersSize = 2_000
	sweepGenBatch    = 512
)

// benchmarkGeneration measures the rate at which a stream can be consumed, which is the rate a
// deployment could offer with the same settings.
func benchmarkGeneration(b *testing.B, profile *Profile, options *StreamOptions) {
	b.Helper()
	stream, err := NewTxStream(profile, options, NewTxCounter(profile.Transaction))
	require.NoError(b, err)

	ctx := b.Context()
	// Start the timer before the service: the stream generates in the background as soon as it
	// runs, and that generation is the workload. Resetting afterwards would let it pre-produce
	// transactions the consume loop then reads for free.
	b.ResetTimer()
	test.RunServiceForTest(ctx, b, stream.Run, nil)
	generator := stream.MakeGenerator()

	param := ConsumeParameters{MinItems: profile.Block.MinSize}
	var consumed int
	for consumed < b.N {
		param.RequestedItems = min(profile.Block.MaxSize, uint64(b.N-consumed)) //nolint:gosec // int -> uint64.
		consumed += len(generator.Consume(ctx, param))
	}
	b.StopTimer()
	test.ReportTxPerSecond(b)
}

func sweepProfile(b *testing.B, workers uint32, scheme signature.Scheme) *Profile {
	b.Helper()
	p := DefaultProfile(workers)
	p.Block.MaxSize = sweepBlockSize
	p.Policy.NamespacePolicies[DefaultGeneratedNamespaceID].Scheme = scheme
	return p
}

func sweepOptions(genBatch uint32, buffersSize int) *StreamOptions {
	return &StreamOptions{BuffersSize: buffersSize, GenBatch: genBatch}
}

// BenchmarkGenerationWorkers finds where added workers stop buying generation rate, per scheme.
// NONE is the floor with signing removed entirely, so the gap to it is what signing costs.
func BenchmarkGenerationWorkers(b *testing.B) {
	flogging.ActivateSpec("fatal")
	for _, scheme := range []signature.Scheme{signature.NoScheme, signature.Eddsa, signature.Ecdsa} {
		for _, workers := range []uint32{8, 16, 32, 64, 128, 256} {
			name := scheme + "/workers-" + strconv.FormatUint(uint64(workers), 10)
			b.Run(name, func(b *testing.B) {
				benchmarkGeneration(b, sweepProfile(b, workers, scheme),
					sweepOptions(sweepGenBatch, sweepBuffersSize))
			})
		}
	}
}

// BenchmarkGenerationGenBatch isolates the per-batch cost each worker pays to hand its
// transactions to the shared channel and the rate controller.
func BenchmarkGenerationGenBatch(b *testing.B) {
	flogging.ActivateSpec("fatal")
	for _, genBatch := range []uint32{1, 100, 512, 4096, 16384} {
		name := "genbatch-" + strconv.FormatUint(uint64(genBatch), 10)
		b.Run(name, func(b *testing.B) {
			benchmarkGeneration(b, sweepProfile(b, 64, signature.Eddsa),
				sweepOptions(genBatch, sweepBuffersSize))
		})
	}
}

// BenchmarkGenerationBuffers isolates the depth of the channels between the workers and the
// consumer, which is what a worker blocks on when the consumer is behind.
func BenchmarkGenerationBuffers(b *testing.B) {
	flogging.ActivateSpec("fatal")
	for _, buffers := range []int{1, 100, 2000, 20000} {
		name := "buffers-" + strconv.Itoa(buffers)
		b.Run(name, func(b *testing.B) {
			benchmarkGeneration(b, sweepProfile(b, 64, signature.Eddsa),
				sweepOptions(sweepGenBatch, buffers))
		})
	}
}
