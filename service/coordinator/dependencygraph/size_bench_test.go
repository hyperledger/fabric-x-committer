/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package dependencygraph

import (
	"fmt"
	"testing"

	"github.com/hyperledger/fabric-lib-go/common/flogging"

	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

// benchSizeBatch is the batch size the coordinator's own pipeline uses on the evaluation cluster.
const benchSizeBatch = 1024

// BenchmarkDependencyGraphBySize measures the graph's cost per transaction as the transaction grows,
// which is the sweep the Fabric-X paper's Figure 9a reports: throughput fell 41% from one
// input/output to four, and the paper attributes it to the dependency graph -- "a synchronization
// primitive protecting the graph serializes access".
//
// Every key here is fresh, so there are no dependencies to resolve. What is left is the graph's
// bookkeeping per key: that isolates growth caused by the number of operations from growth caused by
// contention between them, and only the first can explain a fall on a conflict-free workload.
//
// Unlike BenchmarkDependencyGraph this does not simulate validation latency. That benchmark holds
// each batch for ten seconds to build a large waiting set, which also means a dependent shape
// serialises behind it and a large -benchtime never finishes. Here validated batches are returned
// immediately, so the measurement is the graph's own throughput.
func BenchmarkDependencyGraphBySize(b *testing.B) {
	flogging.ActivateSpec("fatal")

	for _, shape := range []struct {
		name       string
		readWrite  uint32
		blindWrite uint32
	}{
		// The paper's n inputs / n outputs, as read-write operations: each is one read and one
		// write of the same key, and it is the knob every published figure from this cluster uses.
		{name: "rw=1", readWrite: 1},
		{name: "rw=2", readWrite: 2},
		{name: "rw=3", readWrite: 3},
		{name: "rw=4", readWrite: 4},
		// The literal UTXO reading of the same shape: n inputs spent, n outputs created, so 2n
		// keys. It costs the graph twice as many keys per transaction as rw=n.
		{name: "rw=1,bw=1", readWrite: 1, blindWrite: 1},
		{name: "rw=4,bw=4", readWrite: 4, blindWrite: 4},
	} {
		for _, kind := range []string{managerKindSimple, managerKindGlobalLocal} {
			profile := workload.DefaultProfile(1)
			profile.Transaction.ReadOnlyCount = 0
			profile.Transaction.ReadWriteCount = shape.readWrite
			profile.Transaction.BlindWriteCount = shape.blindWrite
			b.Run(fmt.Sprintf("%s/%s", shape.name, kind), func(b *testing.B) {
				benchmarkGraphShape(b, kind, profile)
			})
		}
	}
}

// benchmarkGraphShape times one manager against one transaction shape: it feeds batches in and
// returns every released batch straight back as validated, so the graph is the only thing measured.
func benchmarkGraphShape(b *testing.B, kind string, profile *workload.Profile) {
	b.Helper()
	in := make(chan *TransactionBatch, 8)
	out := make(chan TxNodeBatch, 8)
	val := make(chan TxNodeBatch, 8)
	startManager(b, kind, &Parameters{
		IncomingTxs:               in,
		OutgoingDepFreeTxsNode:    out,
		IncomingValidatedTxsNode:  val,
		NumOfLocalDepConstructors: 4,
		WaitingTxsLimit:           1_000_000,
		PrometheusMetricsProvider: monitoring.NewProvider(),
	})

	txPool := workload.GenerateTransactions(b, profile, b.N+benchSizeBatch)
	ctx := b.Context()
	inWriter := channel.NewWriter(ctx, in)
	outReader := channel.NewReader(ctx, out)
	valWriter := channel.NewWriter(ctx, val)

	b.ReportAllocs()
	b.ResetTimer()
	go func() {
		var id uint64 = 1 // batch IDs start at 1, as the coordinator's do
		for ctx.Err() == nil && len(txPool) > 0 {
			take := min(benchSizeBatch, len(txPool))
			batch := workload.MapToCoordinatorBatch(id, txPool[:take])
			txPool = txPool[take:]
			inWriter.Write(&TransactionBatch{ID: id, Txs: batch.Txs})
			id++
		}
	}()
	for released := 0; released < b.N; {
		batch, ok := outReader.Read()
		if !ok {
			b.Fatal("the graph stopped releasing transactions")
		}
		released += len(batch)
		valWriter.Write(batch)
	}
	b.StopTimer()
	test.ReportTxPerSecond(b)
}
