/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package dependencygraph

import (
	"context"
	"fmt"
	"math/rand/v2"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

// These tests check the invariant a dependency graph manager owes its caller: every transaction it
// accepts is eventually released, and it forgets the ones it has seen validated. When either breaks,
// the graph stops draining and the pipeline halts with no error logged anywhere.
const (
	drainKeySpace   = 32
	drainBatchSize  = 16
	drainBatchCount = 100

	// The saturation test needs channels far smaller than the load it applies, so that every
	// buffer between the graph's output and its validated input is full at once.
	ringChanCap    = 2
	ringBatchSize  = 8
	ringBatchCount = 300
	ringKeysPerTx  = 4
	// A group larger than cap(validatedTxs) + cap(preProcessedValidatedBatchQueue) + the batches
	// the goroutines between them hold.
	ringGroupSize   = 8
	ringGroupCutoff = 5 * time.Millisecond

	// The retention test needs enough transactions that holding all of them is unmistakable.
	forgetTxCount   = 4000
	forgetBatchSize = 8

	// Many more batches than the simple manager's queues hold.
	backpressureBatchCount = 64

	drainTimeout    = 30 * time.Second
	drainPollPeriod = 100 * time.Millisecond
)

// TestManagerDrainsEverySubmittedTx submits a long randomised stream over a key space small
// enough to force dependencies, feeds every released transaction straight back as validated, and
// requires that the graph ends up empty. A manager that loses track of a release leaves a
// residue here.
//
// The with-rejected cases add transactions the coordinator rejects before the graph sees them.
// They reach the validator-committer directly and come back on the validated stream (see
// NewRejectedTransactionNode and the coordinator's rejected path), so a manager that counts
// validated transactions rather than identifying them will drift.
//
// It does not run in parallel: under the race detector on a small machine, the global manager takes
// most of drainTimeout on its own, and sharing the CPUs with the package's other tests pushes it past.
//
//nolint:gocognit,paralleltest // a single test body by design; serial, as explained above.
func TestManagerDrainsEverySubmittedTx(t *testing.T) {
	for _, tc := range []struct { //nolint:paralleltest // serial, as explained above.
		name          string
		manType       string
		rejectedCount int
	}{
		{name: managerKindGlobalLocal, manType: managerKindGlobalLocal},
		{name: managerKindSimple, manType: managerKindSimple},
		{
			name:          managerKindGlobalLocal + "-with-rejected",
			manType:       managerKindGlobalLocal,
			rejectedCount: drainBatchCount / 4,
		},
		{
			name:          managerKindSimple + "-with-rejected",
			manType:       managerKindSimple,
			rejectedCount: drainBatchCount / 4,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			keys := makeTestKeys(t, drainKeySpace)

			incomingTxs := make(chan *TransactionBatch, 64)
			outgoingTxs := make(chan TxNodeBatch, 64)
			validatedTxs := make(chan TxNodeBatch, 64)

			metrics := startManager(t, tc.manType, &Parameters{
				IncomingTxs:               incomingTxs,
				OutgoingDepFreeTxsNode:    outgoingTxs,
				IncomingValidatedTxsNode:  validatedTxs,
				NumOfLocalDepConstructors: 2,
				// High enough that the run never reaches it: this is about losing track of a
				// release, not about backpressure.
				WaitingTxsLimit:           drainBatchSize * drainBatchCount * 2,
				PrometheusMetricsProvider: monitoring.NewProvider(),
			})

			var released atomic.Int64
			go runValidatingDownstream(ctx, outgoingTxs, validatedTxs, &released, false)

			rng := rand.New(rand.NewPCG(1, 2))
			submitted := 0
			in := channel.NewWriter(ctx, incomingTxs)
			back := channel.NewWriter(ctx, validatedTxs)
			for batchNum := range drainBatchCount {
				// Rejected transactions are interleaved with the graph's own, and all of them are
				// in before the assertion, so the drift they would cause cannot be missed by
				// finishing early.
				if batchNum < tc.rejectedCount {
					require.True(t, back.Write(TxNodeBatch{newRejectedTxNodeForTest()}))
				}

				txs := make([]*servicepb.TxWithRef, drainBatchSize)
				for i := range txs {
					// Keys overlap between transactions, so most have to wait for an earlier one,
					// and sometimes repeat within one, which a manager must not mistake for a
					// dependency.
					picked := make([][]byte, 5)
					for k := range picked {
						picked[k] = keys[rng.IntN(len(keys))]
					}
					txs[i] = createTxForTest(t, i, nsID1ForTest, picked[0:2], picked[2:4], picked[4:5])
				}
				require.True(t, in.Write(&TransactionBatch{ID: batchID(batchNum), Txs: txs}))
				submitted += len(txs)
			}

			requireEveryTxReleased(t, metrics, &released, submitted)
			require.EventuallyWithT(t, func(ct *assert.CollectT) {
				require.Equal(ct, submitted, test.GetIntMetricValue(ct, metrics.gdgValidatedTxProcessedTotal),
					"rejected transactions pass through the graph and are not counted as processed")
			}, drainTimeout, drainPollPeriod)
		})
	}
}

// TestManagerSaturatedPipelineKeepsDraining applies more load than the pipeline around the graph
// can hold, so that every buffer in it is full at the same time.
//
// The coordinator's queues form a ring: the graph's output reaches the verifiers and the
// vcservices, and their results return on the graph's validated input. That ring is bounded end to
// end, and its far side stops consuming the graph's output as soon as its own results have nowhere
// to go, because a gRPC stream whose reader has stopped stops its writer through flow control. A
// manager whose single goroutine both writes the output and drains the validated input therefore
// closes the ring: it blocks on a full output, so it never takes the validated batch that would
// let the output drain. Nothing errors, no goroutine dies, and the pipeline never moves again.
func TestManagerSaturatedPipelineKeepsDraining(t *testing.T) {
	t.Parallel()

	for _, manType := range []string{managerKindGlobalLocal, managerKindSimple} {
		t.Run(manType, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()

			incomingTxs := make(chan *TransactionBatch, ringChanCap)
			outgoingTxs := make(chan TxNodeBatch, ringChanCap)
			validatedTxs := make(chan TxNodeBatch, ringChanCap)

			metrics := startManager(t, manType, &Parameters{
				IncomingTxs:               incomingTxs,
				OutgoingDepFreeTxsNode:    outgoingTxs,
				IncomingValidatedTxsNode:  validatedTxs,
				NumOfLocalDepConstructors: 2,
				// Far more than the pipeline can hold, so the limit never engages and cannot be
				// what keeps the graph draining.
				WaitingTxsLimit:           ringBatchSize * ringBatchCount * 2,
				PrometheusMetricsProvider: monitoring.NewProvider(),
			})

			var released atomic.Int64
			go runGroupingDownstream(ctx, outgoingTxs, validatedTxs, &released)

			// Every transaction holds keys of its own, so none of them waits for another. The
			// only key they share is the namespace key that every transaction reads.
			in := channel.NewWriter(ctx, incomingTxs)
			go func() {
				for batchNum := range ringBatchCount {
					txs := make([]*servicepb.TxWithRef, ringBatchSize)
					for i := range txs {
						keys := makeUniqueTestKeys(batchNum*ringBatchSize+i, ringKeysPerTx)
						txs[i] = createTxForTest(t, i, nsID1ForTest, keys[0:1], keys[1:3], keys[3:4])
					}
					if !in.Write(&TransactionBatch{ID: batchID(batchNum), Txs: txs}) {
						return
					}
				}
			}()

			requireEveryTxReleased(t, metrics, &released, ringBatchCount*ringBatchSize)
		})
	}
}

// TestManagerForgetsValidatedTxs requires that the graph stops referencing a transaction it has
// released and seen validated. Otherwise memory grows with the number of transactions committed
// rather than with the number waiting.
//
// Every transaction reads the key of its namespace, so they all share one key, as readers, for the
// whole life of the load. The simple manager is the case in point: if it kept the members of that
// key's in-flight group rather than counting them, it would keep every transaction that ever ran,
// because the group closes only in the instant when nothing is in flight, which never comes under
// sustained load.
func TestManagerForgetsValidatedTxs(t *testing.T) {
	t.Parallel()

	for _, manType := range []string{managerKindGlobalLocal, managerKindSimple} {
		t.Run(manType, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()

			incomingTxs := make(chan *TransactionBatch, 8)
			outgoingTxs := make(chan TxNodeBatch, 8)
			validatedTxs := make(chan TxNodeBatch, 8)

			metrics := startManager(t, manType, &Parameters{
				IncomingTxs:               incomingTxs,
				OutgoingDepFreeTxsNode:    outgoingTxs,
				IncomingValidatedTxsNode:  validatedTxs,
				NumOfLocalDepConstructors: 1,
				WaitingTxsLimit:           forgetTxCount * 2,
				PrometheusMetricsProvider: monitoring.NewProvider(),
			})

			var released atomic.Int64
			go runValidatingDownstream(ctx, outgoingTxs, validatedTxs, &released, true)

			// A cleanup fires once nothing references the transaction any more. It must not close
			// over the transaction it is attached to, or it would keep it alive forever.
			var collected atomic.Int64
			in := channel.NewWriter(ctx, incomingTxs)
			submitted := 0
			for batchNum := range forgetTxCount / forgetBatchSize {
				txs := make([]*servicepb.TxWithRef, forgetBatchSize)
				for i := range txs {
					keys := makeUniqueTestKeys(batchNum*forgetBatchSize+i, ringKeysPerTx)
					txs[i] = createTxForTest(t, i, nsID1ForTest, keys[0:1], keys[1:3], keys[3:4])
					runtime.AddCleanup(txs[i], func(int) { collected.Add(1) }, 0)
				}
				require.True(t, in.Write(&TransactionBatch{ID: batchID(batchNum), Txs: txs}))
				submitted += len(txs)
			}

			// Everything but the batch the stand-in holds back must be released and validated.
			expectedReleased := int64(submitted - forgetBatchSize)
			require.EventuallyWithT(t, func(ct *assert.CollectT) {
				require.Equal(ct, expectedReleased, released.Load())
				require.Equal(ct, forgetBatchSize, test.GetIntMetricValue(ct, metrics.gdgWaitingTxCount))
			}, drainTimeout, drainPollPeriod)

			require.EventuallyWithT(t, func(ct *assert.CollectT) {
				runtime.GC()
				// A handful may still be reachable from a stack or from a queue that has not been
				// reused.
				require.Greater(ct, collected.Load(), expectedReleased*3/4)
			}, drainTimeout, drainPollPeriod)
		})
	}
}

// TestManagerReleasesNamespaceLifecycleTx requires that a namespace lifecycle transaction is released.
//
// The transaction carries the _meta namespace with a read-write on ns1 and the ns1 namespace with a
// blind write, so before the fix it produced _meta:ns1 as both a reads-and-writes key and a reads-only
// key. The simple manager called checkTXFree for that key twice, once as the writer that created the
// in-flight group and once as a reader that had to wait for it, so the transaction waited on itself and
// the key was never freed. The default manager could not reach this, because getDependenciesOf runs
// before addWaitingTx and a transaction is never in the detector when its own dependencies are
// computed -- hence both kinds run here, one as the regression and one as the control.
func TestManagerReleasesNamespaceLifecycleTx(t *testing.T) {
	t.Parallel()

	for _, manType := range []string{managerKindGlobalLocal, managerKindSimple} {
		t.Run(manType, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()

			incomingTxs := make(chan *TransactionBatch, 1)
			outgoingTxs := make(chan TxNodeBatch, 1)
			validatedTxs := make(chan TxNodeBatch, 1)

			metrics := startManager(t, manType, &Parameters{
				IncomingTxs:               incomingTxs,
				OutgoingDepFreeTxsNode:    outgoingTxs,
				IncomingValidatedTxsNode:  validatedTxs,
				NumOfLocalDepConstructors: 1,
				WaitingTxsLimit:           8,
				PrometheusMetricsProvider: monitoring.NewProvider(),
			})

			var released atomic.Int64
			go runValidatingDownstream(ctx, outgoingTxs, validatedTxs, &released, false)

			in := channel.NewWriter(ctx, incomingTxs)
			require.True(t, in.Write(&TransactionBatch{
				ID:  batchID(0),
				Txs: []*servicepb.TxWithRef{createNamespaceLifecycleTxForTest(nsID1ForTest, []byte("k"))},
			}))

			requireEveryTxReleased(t, metrics, &released, 1)
		})
	}
}

// TestManagerReleasesTxRepeatingItsOwnKey requires that a transaction naming one key more than once
// is released. The namespace lifecycle transaction above is one way to get there; this covers any
// other, so the simple manager does not rely on key derivation never producing a repeat.
func TestManagerReleasesTxRepeatingItsOwnKey(t *testing.T) {
	t.Parallel()

	k := [][]byte{[]byte("k")}
	for _, manType := range []string{managerKindGlobalLocal, managerKindSimple} {
		t.Run(manType, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()

			incomingTxs := make(chan *TransactionBatch, 1)
			outgoingTxs := make(chan TxNodeBatch, 1)
			validatedTxs := make(chan TxNodeBatch, 1)

			metrics := startManager(t, manType, &Parameters{
				IncomingTxs:               incomingTxs,
				OutgoingDepFreeTxsNode:    outgoingTxs,
				IncomingValidatedTxsNode:  validatedTxs,
				NumOfLocalDepConstructors: 1,
				WaitingTxsLimit:           8,
				PrometheusMetricsProvider: monitoring.NewProvider(),
			})

			var released atomic.Int64
			go runValidatingDownstream(ctx, outgoingTxs, validatedTxs, &released, false)

			// The second transaction waits for the first on k, so a first one that is never
			// released, or released twice from k's queue, shows up in both counts.
			in := channel.NewWriter(ctx, incomingTxs)
			require.True(t, in.Write(&TransactionBatch{
				ID: batchID(0),
				Txs: []*servicepb.TxWithRef{
					createTxForTest(t, 0, nsID1ForTest, k, k, k),
					createTxForTest(t, 1, nsID1ForTest, nil, k, nil),
				},
			}))

			requireEveryTxReleased(t, metrics, &released, 2)
		})
	}
}

// TestSimpleManagerStopsTakingInputWhileOutputIsFull requires that the simple manager stops
// accepting transactions once its output is not being read. Its released batches wait in a buffer
// rather than in a blocking write, so that the validated input keeps moving (see
// TestManagerSaturatedPipelineKeepsDraining); without a pause on input, that buffer grows up to the
// waiting limit, which can be set far beyond what fits in memory.
func TestSimpleManagerStopsTakingInputWhileOutputIsFull(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	incomingTxs := make(chan *TransactionBatch, 1)
	outgoingTxs := make(chan TxNodeBatch, 1)

	metrics := startManager(t, managerKindSimple, &Parameters{
		IncomingTxs:               incomingTxs,
		OutgoingDepFreeTxsNode:    outgoingTxs,
		IncomingValidatedTxsNode:  make(chan TxNodeBatch, 1),
		NumOfLocalDepConstructors: 1,
		WaitingTxsLimit:           backpressureBatchCount * forgetBatchSize * 2,
		PrometheusMetricsProvider: monitoring.NewProvider(),
	})

	// Nothing reads the output. Every transaction is independent, so each batch is released whole.
	in := channel.NewWriter(ctx, incomingTxs)
	go func() {
		for batchNum := range backpressureBatchCount {
			txs := make([]*servicepb.TxWithRef, forgetBatchSize)
			for i := range txs {
				keys := makeUniqueTestKeys(batchNum*forgetBatchSize+i, ringKeysPerTx)
				txs[i] = createTxForTest(t, i, nsID1ForTest, nil, keys, nil)
			}
			if !in.Write(&TransactionBatch{ID: batchID(batchNum), Txs: txs}) {
				return
			}
		}
	}()

	// One batch in the output channel and one in the buffer; the rest stay queued upstream.
	maxProcessed := 2 * forgetBatchSize
	require.Eventually(t, func() bool {
		return test.GetIntMetricValue(t, metrics.gdgTxProcessedTotal) == maxProcessed
	}, drainTimeout, time.Millisecond)
	require.Never(t, func() bool {
		return test.GetIntMetricValue(t, metrics.gdgTxProcessedTotal) > maxProcessed
	}, 200*time.Millisecond, 5*time.Millisecond)
}

// runValidatingDownstream stands in for the pipeline that follows the graph: whatever the graph
// frees is immediately validated. With holdFirst it never validates the first batch it is given, so
// the namespace key that every transaction reads keeps an in-flight group for the whole test, as it
// does under sustained load. The count is an atomic rather than a channel so that this never blocks on the
// assertions -- a stand-in that stops reading the output deadlocks the manager it is testing.
func runValidatingDownstream( //nolint:revive // false positive: control flag.
	ctx context.Context, outgoingTxs <-chan TxNodeBatch, validatedTxs chan<- TxNodeBatch, released *atomic.Int64,
	holdFirst bool,
) {
	out := channel.NewReader(ctx, outgoingTxs)
	back := channel.NewWriter(ctx, validatedTxs)
	for {
		batch, ok := out.Read()
		if !ok {
			return
		}
		if holdFirst {
			holdFirst = false
			continue
		}
		released.Add(int64(len(batch)))
		if !back.Write(batch) {
			return
		}
	}
}

// runGroupingDownstream stands in for the verifiers and the vcservices as one: they take batches
// from the graph, work on a group of them together, and return the group's results in a lump,
// cutting the group on size or on time as a vcservice does (see MinTransactionBatchSize and
// TimeoutForMinTransactionBatchSize). While returning a lump they take nothing new, which is what a
// gRPC stream does to its writer once its reader has stopped. A group is larger than every queue
// between the graph's output and its validated input, so a lump cannot be returned unless the graph
// keeps accepting validated batches while its own output waits.
func runGroupingDownstream(
	ctx context.Context, outgoingTxs <-chan TxNodeBatch, validatedTxs chan<- TxNodeBatch, released *atomic.Int64,
) {
	back := channel.NewWriter(ctx, validatedTxs)
	for ctx.Err() == nil {
		group := make([]TxNodeBatch, 0, ringGroupSize)
		cutoff := time.After(ringGroupCutoff)
		for len(group) < ringGroupSize {
			select {
			case batch := <-outgoingTxs:
				group = append(group, batch)
				continue
			case <-cutoff:
			case <-ctx.Done():
			}
			break
		}
		for _, batch := range group {
			released.Add(int64(len(batch)))
			if !back.Write(batch) {
				return
			}
		}
	}
}

// newRejectedTxNodeForTest builds a transaction the coordinator rejected before the graph saw it.
// Such a transaction never entered the graph, so it carries no keys and no waiters.
func newRejectedTxNodeForTest() *TransactionNode {
	return NewRejectedTransactionNode(&committerpb.TxStatus{
		Ref:    committerpb.NewTxRef(uuid.New().String(), 0, 0),
		Status: committerpb.Status_ABORTED_SIGNATURE_INVALID,
	})
}

// requireEveryTxReleased waits for the graph to release everything it was given. The graph's own
// gauge is the clearest statement of the invariant -- nothing left waiting -- but on its own it
// also holds before the first transaction arrives, so the released count is checked with it.
func requireEveryTxReleased(t *testing.T, metrics *perfMetrics, released *atomic.Int64, submitted int) {
	t.Helper()
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		require.Equal(ct, int64(submitted), released.Load(),
			"every submitted transaction must be released exactly once")
		require.Equal(ct, 0, test.GetIntMetricValue(ct, metrics.gdgWaitingTxCount),
			"no transaction may be left waiting")
		require.Equal(ct, 0, test.GetIntMetricValue(ct, metrics.gdgDepFreeTxCount),
			"no released batch may be left unsent")
	}, drainTimeout, drainPollPeriod)
}

// makeUniqueTestKeys returns n keys used by no other transaction.
func makeUniqueTestKeys(txNum, n int) [][]byte {
	keys := make([][]byte, n)
	for i := range keys {
		keys[i] = fmt.Appendf(nil, "%d-%d", txNum, i)
	}
	return keys
}

// batchID numbers a batch as the coordinator's txBatchIDToDepGraph does. The local dependency
// constructor orders its output by this ID and expects the first one to be 1: a batch numbered 0
// waits forever for a predecessor that cannot exist.
func batchID(batchNum int) uint64 {
	return uint64(batchNum) + 1 //nolint:gosec // int -> uint64.
}

// createNamespaceLifecycleTxForTest builds a namespace lifecycle transaction, which names _meta:<ns>
// twice: a read-write on ns through the _meta namespace, which is how a policy change is expressed, plus a
// blind write inside ns itself, which adds the implicit read of _meta:<ns>.
func createNamespaceLifecycleTxForTest(nsID string, key []byte) *servicepb.TxWithRef {
	return &servicepb.TxWithRef{
		Ref: committerpb.NewTxRef(uuid.New().String(), 0, 0),
		Content: &applicationpb.Tx{
			Namespaces: []*applicationpb.TxNamespace{
				{
					NsId:       committerpb.MetaNamespaceID,
					ReadWrites: []*applicationpb.ReadWrite{{Key: []byte(nsID)}},
				},
				{
					NsId:        nsID,
					BlindWrites: []*applicationpb.Write{{Key: key}},
				},
			},
		},
	}
}
