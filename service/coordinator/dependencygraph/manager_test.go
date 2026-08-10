/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package dependencygraph

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/hyperledger/fabric-lib-go/common/flogging"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

const DefaultQueueMonitorSamplingTime = 100 * time.Millisecond

// Manager kinds under test.
const (
	managerKindGlobalLocal = "global-local"
	managerKindSimple      = "simple"
)

type batchWithLatency struct {
	batch TxNodeBatch
	done  time.Time
}

//nolint:gocognit // single method for simplicity.
func BenchmarkDependencyGraph(b *testing.B) {
	flogging.ActivateSpec("fatal")

	// Parameters
	latency := 10 * time.Second
	batchSize := 1024

	// Dependency shapes over the united key space: writes CREATE keys (grow the space) and every
	// remaining write slot plus every read-only slot BACKWARD-references a past create (reuse).
	// key-backref-rate is the number of backward references per transaction; the remaining slots create
	// keys. Forward (read-before-create / write-after-read) dependencies are NOT synthesized statically —
	// at runtime they arise from out-of-order commit — so this benchmark exercises the backward shapes.
	// configure mutates the whole *workload.Profile (it sets p.Transaction.* fields).
	type shape struct {
		name      string
		configure func(p *workload.Profile)
	}
	// noSplit leaves key-backref-rate at 0: every slot is a fresh unique key, so there are no dependencies.
	noSplit := func(ro, rw, bw uint32) func(*workload.Profile) {
		return func(p *workload.Profile) {
			p.Transaction.ReadOnlyCount = ro
			p.Transaction.ReadWriteCount = rw
			p.Transaction.BlindWriteCount = bw
		}
	}
	// withSplit enables references: one create per transaction, the rest drawn from a 64-key working set
	// one transaction behind.
	withSplit := func(ro, rw, bw uint32) func(*workload.Profile) {
		return func(p *workload.Profile) {
			p.Transaction.ReadOnlyCount = ro
			p.Transaction.ReadWriteCount = rw
			p.Transaction.BlindWriteCount = bw
			p.Transaction.KeyBackrefRate = float64(ro+rw+bw) - 1 // all but one slot references a past create
			p.Transaction.TxReferenceGap = 1
			p.Transaction.KeyLookbackWindow = 64
		}
	}
	shapes := []shape{
		{"no-dep", noSplit(1, 2, 1)},              // fresh unique keys, no deps
		{"write__read", withSplit(1, 0, 1)},       // BW create; backward RO ref -> RAW
		{"write__write", withSplit(0, 0, 2)},      // BW create + backward BW ref -> WAW
		{"write__read-write", withSplit(0, 2, 0)}, // RW create + backward RW ref -> RAW+WAW
	}

	for _, sh := range shapes {
		p := workload.DefaultProfile(1)
		sh.configure(p)

		b.Run(sh.name, func(b *testing.B) {
			for _, tc := range []struct {
				kind    string
				workers int
			}{
				{kind: managerKindSimple, workers: 2},
				{kind: managerKindGlobalLocal, workers: 2},
				{kind: managerKindGlobalLocal, workers: 4},
			} {
				name := fmt.Sprintf("%s-%d", tc.kind, tc.workers)
				b.Run(name, func(b *testing.B) {
					// The main queues.
					in := make(chan *TransactionBatch, 8)
					out := make(chan TxNodeBatch, 8)
					val := make(chan TxNodeBatch, 8)

					// Starts the dependency manager.
					startManager(b, tc.kind, &Parameters{
						IncomingTxs:               in,
						OutgoingDepFreeTxsNode:    out,
						IncomingValidatedTxsNode:  val,
						NumOfLocalDepConstructors: tc.workers,
						WaitingTxsLimit:           20_000_000,
						QueueMonitorSamplingTime:  DefaultQueueMonitorSamplingTime,
						PrometheusMetricsProvider: monitoring.NewProvider(),
					})

					// Over-supply transactions (3x) so the manager's internal
					// queue stays populated throughout the measured window; we
					// stop once b.N transactions have been released.
					txPoll := workload.GenerateTransactions(b, p, max(b.N*3, batchSize*3))

					ctx := b.Context()
					outCtx := channel.NewReader(ctx, out)
					valCtx := channel.NewWriter(ctx, val)
					inCtx := channel.NewWriter(ctx, in)
					latencySimulatorQueue := channel.Make[*batchWithLatency](ctx, 1024*1024)

					// Simulates the batch processing latency.
					go func() {
						for ctx.Err() == nil {
							wtl, ok := latencySimulatorQueue.Read()
							if !ok {
								return
							}
							waitFor := time.Until(wtl.done)
							if waitFor > 0 {
								select {
								case <-time.After(waitFor):
								case <-ctx.Done():
								}
							}
							valCtx.Write(wtl.batch)
						}
					}()

					b.ResetTimer()
					// Generates the load to the manager's queue.
					go func() {
						var i uint64
						for ctx.Err() == nil && len(txPoll) > 0 {
							take := min(batchSize, len(txPoll))
							batch := workload.MapToCoordinatorBatch(i, txPoll[:take])
							txPoll = txPoll[take:]
							inCtx.Write(&TransactionBatch{
								ID:  i,
								Txs: batch.Txs,
							})
							i++
						}
					}()
					// Reads the output of the manager, and forward it to the latency simulator.
					var total int
					for total < b.N {
						batch, ok := outCtx.Read()
						if !ok {
							return
						}
						latencySimulatorQueue.Write(&batchWithLatency{
							batch: batch,
							done:  time.Now().Add(latency),
						})
						total += len(batch)
					}
					b.StopTimer()
					test.ReportTxPerSecond(b)
				})
			}
		})
	}
}

func TestDependencyGraphManager(t *testing.T) {
	t.Parallel()

	keysPoll := makeTestKeys(t, 100)
	keys := func(idx ...int) [][]byte {
		k := make([][]byte, len(idx))
		for i := range idx {
			k[i] = keysPoll[idx[i]]
		}
		return k
	}

	const waitingTXsLimit = 20

	for _, manType := range []string{managerKindGlobalLocal, managerKindSimple} {
		t.Run(manType, func(t *testing.T) {
			t.Parallel()
			incomingTxs := make(chan *TransactionBatch, 10)
			outgoingTxs := make(chan TxNodeBatch, 10)
			validatedTxs := make(chan TxNodeBatch, 10)

			metrics := startManager(t, manType, &Parameters{
				IncomingTxs:               incomingTxs,
				OutgoingDepFreeTxsNode:    outgoingTxs,
				IncomingValidatedTxsNode:  validatedTxs,
				NumOfLocalDepConstructors: 2,
				WaitingTxsLimit:           waitingTXsLimit,
				QueueMonitorSamplingTime:  DefaultQueueMonitorSamplingTime,
				PrometheusMetricsProvider: monitoring.NewProvider(),
			})

			t.Log("check reads and writes dependency tracking")
			test.RequireIntMetricValue(t, 0, metrics.dependentTransactionsQueueSize)

			// t2 depends on t1
			t1 := createTxForTest(t, 0, nsID1ForTest, keys(0, 1), keys(2, 3), keys(4, 5))
			t2 := createTxForTest(t, 1, nsID1ForTest, keys(4, 5), keys(2, 6), keys(3, 7))

			incomingTxs <- &TransactionBatch{
				ID:  1,
				Txs: []*servicepb.TxWithRef{t1, t2},
			}

			test.EventuallyIntMetric(t, 1, metrics.dependentTransactionsQueueSize, 5*time.Second, 100*time.Millisecond)

			// t3 depends on t2 and t1
			t3 := createTxForTest(t, 0, nsID1ForTest, keys(7), keys(2, 3), keys(8, 5))
			// t4 depends on t2 and t1
			t4 := createTxForTest(t, 1, nsID1ForTest, keys(7, 6), keys(4, 1), keys(0, 9))

			incomingTxs <- &TransactionBatch{
				ID:  2,
				Txs: []*servicepb.TxWithRef{t3, t4},
			}

			test.EventuallyIntMetric(t, 3, metrics.dependentTransactionsQueueSize, 5*time.Second, 100*time.Millisecond)

			// only t1 is dependency free
			depFreeTxs := <-outgoingTxs
			require.Len(t, depFreeTxs, 1)
			actualT1 := depFreeTxs[0]
			test.RequireProtoEqual(t, t1.Ref, actualT1.VCTx.Ref)
			ensureNoOutputs(t, outgoingTxs)

			validatedTxs <- TxNodeBatch{actualT1}

			// after t1 is validated, t2 is dependency free
			depFreeTxs = <-outgoingTxs
			require.Len(t, depFreeTxs, 1)
			actualT2 := depFreeTxs[0]
			test.RequireProtoEqual(t, t2.Ref, actualT2.VCTx.Ref)
			ensureNoOutputs(t, outgoingTxs)

			test.RequireIntMetricValue(t, 2, metrics.dependentTransactionsQueueSize)

			validatedTxs <- TxNodeBatch{actualT2}

			// after t2 is validated, both t3 and t4 are dependency free
			depFreeTxs = <-outgoingTxs
			require.Len(t, depFreeTxs, 2)
			var actualT3, actualT4 *TransactionNode
			if t3.Ref.TxId == depFreeTxs[0].VCTx.Ref.TxId {
				actualT3 = depFreeTxs[0]
				actualT4 = depFreeTxs[1]
			} else {
				actualT3 = depFreeTxs[1]
				actualT4 = depFreeTxs[0]
			}
			test.RequireProtoEqual(t, t3.Ref, actualT3.VCTx.Ref)
			test.RequireProtoEqual(t, t4.Ref, actualT4.VCTx.Ref)
			ensureNoOutputs(t, outgoingTxs)

			test.RequireIntMetricValue(t, 0, metrics.dependentTransactionsQueueSize)

			validatedTxs <- TxNodeBatch{actualT3, actualT4}

			ensureProcessedAndValidatedMetrics(t, metrics, 4, 4)
			// after validating all txs, the dependency detector should be empty
			ensureWaitingTXsLimit(t, metrics, 0)

			t.Log("check dependency in namespace")
			// t2 depends on t1
			t1 = createTxForTest(
				t, 1, committerpb.MetaNamespaceID, nil, [][]byte{[]byte(nsID1ForTest)}, nil,
			)
			t2 = createTxForTest(
				t, 2, nsID1ForTest, keys(4, 5), keys(2, 6), keys(3, 7),
			)

			incomingTxs <- &TransactionBatch{
				ID:  3,
				Txs: []*servicepb.TxWithRef{t1, t2},
			}

			// t3 depends on t2, and t1
			t3 = createTxForTest(
				t, 0, committerpb.MetaNamespaceID, nil, [][]byte{[]byte(nsID1ForTest)}, nil,
			)
			// t4 depends on t3, t2 and t1
			t4 = createTxForTest(t, 1, nsID1ForTest, keys(7, 6), keys(4, 1), keys(0, 9))

			incomingTxs <- &TransactionBatch{
				ID:  4,
				Txs: []*servicepb.TxWithRef{t3, t4},
			}

			// only t1 is dependency free
			depFreeTxs = <-outgoingTxs
			require.Len(t, depFreeTxs, 1)
			actualT1 = depFreeTxs[0]
			test.RequireProtoEqual(t, t1.Ref, actualT1.VCTx.Ref)
			ensureNoOutputs(t, outgoingTxs)

			validatedTxs <- TxNodeBatch{actualT1}

			// after t1 is validated, t2 is dependency free
			depFreeTxs = <-outgoingTxs
			require.Len(t, depFreeTxs, 1)
			actualT2 = depFreeTxs[0]
			test.RequireProtoEqual(t, t2.Ref, actualT2.VCTx.Ref)
			ensureNoOutputs(t, outgoingTxs)

			validatedTxs <- TxNodeBatch{actualT2}

			// after t2 is validated, t3 becomes dependency free
			depFreeTxs = <-outgoingTxs
			require.Len(t, depFreeTxs, 1)
			actualT3 = depFreeTxs[0]
			test.RequireProtoEqual(t, t3.Ref, actualT3.VCTx.Ref)
			ensureNoOutputs(t, outgoingTxs)

			validatedTxs <- TxNodeBatch{actualT3}

			// after t3 is validated, t4 becomes dependency free
			depFreeTxs = <-outgoingTxs
			require.Len(t, depFreeTxs, 1)
			actualT4 = depFreeTxs[0]
			test.RequireProtoEqual(t, t4.Ref, actualT4.VCTx.Ref)
			ensureNoOutputs(t, outgoingTxs)

			validatedTxs <- TxNodeBatch{actualT4}

			ensureProcessedAndValidatedMetrics(t, metrics, 8, 8)
			// after validating all txs, the dependency detector should be empty
			ensureWaitingTXsLimit(t, metrics, 0)

			t.Log("check waiting TX limit")
			txs := make([]*servicepb.TxWithRef, waitingTXsLimit+1)
			for i := range txs {
				txs[i] = createTxForTest(t, i, nsID1ForTest, nil, keys(i), nil)
			}
			incomingTxs <- &TransactionBatch{
				ID:  5,
				Txs: txs,
			}

			txs2 := make([]*servicepb.TxWithRef, 10)
			for i := range txs2 {
				txs2[i] = createTxForTest(t, i, nsID1ForTest, nil, keys(i+len(txs)), nil)
			}
			incomingTxs <- &TransactionBatch{
				ID:  6,
				Txs: txs2,
			}

			depFreeTxs = <-outgoingTxs
			require.Len(t, depFreeTxs, len(txs))
			ensureNoOutputs(t, outgoingTxs)

			ensureWaitingTXsLimit(t, metrics, len(txs))
			ensureNeverGreaterWaitingTXsLimit(t, metrics, len(txs))

			validatedTxs <- depFreeTxs
			depFreeTxs = <-outgoingTxs
			require.Len(t, depFreeTxs, len(txs2))
			ensureNoOutputs(t, outgoingTxs)

			ensureWaitingTXsLimit(t, metrics, len(txs2))
			ensureNeverGreaterWaitingTXsLimit(t, metrics, len(txs2))

			validatedTxs <- depFreeTxs
			ensureWaitingTXsLimit(t, metrics, 0)
		})
	}
}

func ensureWaitingTXsLimit(t *testing.T, metrics *perfMetrics, expectedValue int) {
	t.Helper()
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		require.Equal(ct, expectedValue, test.GetIntMetricValue(t, metrics.gdgWaitingTxQueueSize))
	}, 5*time.Second, 100*time.Millisecond)
}

func ensureNeverGreaterWaitingTXsLimit(t *testing.T, metrics *perfMetrics, expectedValue int) {
	t.Helper()
	require.Never(t, func() bool {
		return test.GetIntMetricValue(t, metrics.gdgWaitingTxQueueSize) > expectedValue
	}, 5*time.Second, 100*time.Millisecond)
}

func ensureNoOutputs(t *testing.T, outgoingTxs <-chan TxNodeBatch) {
	t.Helper()
	select {
	case badDepTXs := <-outgoingTxs:
		t.Fatalf("got transactions: %v", badDepTXs)
	case <-time.After(time.Second):
		// Fantastic.
	}
}

func startManager(tb testing.TB, kind string, p *Parameters) *perfMetrics {
	tb.Helper()
	var manService interface {
		Run(context.Context)
	}
	var metrics *perfMetrics
	switch kind {
	case managerKindGlobalLocal:
		m := NewManager(p)
		manService = m
		metrics = m.metrics
	case managerKindSimple:
		m := NewSimpleManager(p)
		manService = m
		metrics = m.metrics
	default:
		return nil
	}
	test.RunServiceForTest(tb.Context(), tb, func(ctx context.Context) error {
		manService.Run(ctx)
		return nil
	}, nil)
	return metrics
}
