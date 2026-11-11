/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package adapters

import (
	"testing"

	"github.com/hyperledger/fabric-lib-go/common/flogging"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-committer/loadgen/metrics"
	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/utils/signature"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

// A deployment's offered rate is bounded by everything on the submit path, not by generation
// alone, and the two can be far apart: on a 19-machine cluster generation benchmarked at
// 585,354 tx/s while the deployment offered 358,800. This benchmark closes that gap by measuring
// the same path a real adapter runs -- generation, block mapping, TX ID extraction and the
// metrics and latency hooks -- with a sender that does nothing.
//
// What it deliberately excludes is the gRPC submission and the status receive path. So comparing
// it against BenchmarkGenerationWorkers in the workload package attributes the difference: a
// result near the generation rate leaves gRPC and receiving as the only suspects, and a result
// near the deployment's offered rate puts the cost in the mapping and metrics layer instead.
const (
	submitBlockSize = 10_000
	submitWorkers   = 128
	// 512, matching the generation sweep this is compared against. A larger batch delays the
	// first delivery: each worker signs a whole batch before writing, so 16,384 across 128
	// workers is 2.1M transactions before anything arrives, several seconds at any real rate.
	submitGenBatch = 512
)

func benchmarkSubmitPath(b *testing.B, sender func(*common.Block) error) {
	b.Helper()
	flogging.ActivateSpec("fatal")

	profile := workload.DefaultProfile(submitWorkers)
	profile.Block.MaxSize = submitBlockSize
	profile.Policy.NamespacePolicies[workload.DefaultGeneratedNamespaceID].Scheme = signature.Eddsa

	streamOptions := &workload.StreamOptions{
		BuffersSize: 100,
		GenBatch:    submitGenBatch,
		// Unlimited: this measures the achievable rate, so a limit would measure the limiter.
		RateLimit: 0,
	}

	txCounter := workload.NewTxCounter(profile.Transaction)
	txStream, err := workload.NewTxStream(profile, streamOptions, txCounter)
	require.NoError(b, err)

	res := &ClientResources{
		// An empty metrics Config leaves the latency buckets empty, which makes the latency
		// tracker a no-op. Deliberate: this measures the submit path, and the tracker's own cost
		// belongs to the metrics package.
		Metrics: metrics.NewLoadgenServiceMetrics(&metrics.Config{}, txCounter, txStream),
		Profile: profile,
		Stream:  streamOptions,
		Limit:   &GenerateLimit{},
	}
	adapter := &commonAdapter{res: res}

	ctx := b.Context()
	// The stream generates in the background as soon as it starts, and that generation is part of
	// the workload, so the timer starts before it.
	b.ResetTimer()
	test.RunServiceForTest(ctx, b, txStream.Run, nil)
	generator := txStream.MakeGenerator()

	// The same steps sendBlocks performs per block, run inline rather than across its channel, so
	// that the benchmark framework can scale the iteration count. Losing the pipelining between
	// mapping and sending makes this a serial cost per transaction, which is what attributing the
	// gap needs.
	param := workload.ConsumeParameters{MinItems: profile.Block.MinSize}
	var consumed int
	for consumed < b.N {
		param.RequestedItems = min(profile.Block.MaxSize, uint64(b.N-consumed)) //nolint:gosec // int -> uint64.
		txs := generator.Consume(ctx, param)
		if len(txs) == 0 {
			break
		}
		block := workload.MapToOrdererBlock(adapter.NextBlockNum(), txs)
		res.Metrics.OnSendBatch(getTXsIDs(txs))
		require.NoError(b, sender(block))
		consumed += len(txs)
	}
	b.StopTimer()
	test.ReportTxPerSecond(b)
}

// BenchmarkSubmitPathNoOpSender measures generation plus everything the adapter does to a block
// before it would hand it to a transport.
func BenchmarkSubmitPathNoOpSender(b *testing.B) {
	benchmarkSubmitPath(b, func(*common.Block) error { return nil })
}

// BenchmarkSubmitPathMarshalSender adds a marshal of the assembled block, which is the work any
// real transport must do, without the transport itself.
func BenchmarkSubmitPathMarshalSender(b *testing.B) {
	benchmarkSubmitPath(b, func(block *common.Block) error {
		_, err := proto.Marshal(block)
		return err
	})
}
