/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package workload

import (
	"context"
	"fmt"
	"testing"

	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/hyperledger/fabric-x-committer/utils/test"
)

// TestFillVersionsAverageConvergesAtBatchSizeOne proves the persistent accumulator makes queries-rate an
// accurate AVERAGE even when every batch has exactly one transaction (GenBatch == 1): a per-call/reset
// accumulator would floor a rate below 1 to zero selections on every single-tx batch, forever. Every
// candidate key is present in the store, so a selection is always a hit — isolating the accumulator's
// selection behavior from hit/miss noise.
func TestFillVersionsAverageConvergesAtBatchSizeOne(t *testing.T) {
	t.Parallel()
	const readsPerTx = 5
	roKeys := make([][]byte, readsPerTx)
	rwKeys := make([][]byte, readsPerTx)
	versions := make(map[string]uint64, 2*readsPerTx)
	for i := range readsPerTx {
		roKeys[i] = []byte(fmt.Sprintf("ro-%d", i))
		rwKeys[i] = []byte(fmt.Sprintf("rw-%d", i))
		versions[string(roKeys[i])] = uint64(i)
		versions[string(rwKeys[i])] = uint64(i)
	}

	const (
		rate = 0.3
		n    = 1000
	)
	stub := &stubKeyVersionQuerier{versions: singleNsVersions(versions)}
	f := newQueryFiller(stub, rate) // one accumulator, reused across every single-tx batch below.

	var totalVersioned int
	for range n {
		tx := newTestTx(roKeys, rwKeys)
		require.NoError(t, f.FillVersions(t.Context(), []*applicationpb.Tx{tx}))
		totalVersioned += countVersioned(tx)
	}

	avg := float64(totalVersioned) / float64(n)
	t.Logf("rate=%v over %d single-tx batches: %d reads versioned (avg %.4f, calls %d)",
		rate, n, totalVersioned, avg, len(stub.calls))
	require.InDelta(t, rate, avg, 0.02,
		"average versioned reads per batch should converge to the configured queries-rate")
	require.Positive(t, totalVersioned,
		"a persistent accumulator must still select reads across batches of size 1")
	require.Less(t, len(stub.calls), n,
		"batches with a zero budget must not issue a GetRows call at all")
}

// TestFillVersionsPriorityAndHitMiss proves the selection order (ReadsOnly then ReadWrites, each from
// last to first) and the hit/miss outcome, using a store that only knows the "back-reference" keys
// (ro-back, rw-back) and not the "new" keys (ro-new, rw-new) — mirroring how back-references may already
// carry a committed version while newly-introduced keys never do. Growing the budget walks the exact
// priority order: read-only back-reference, read-only new (a miss, but still queried), read-write
// back-reference, read-write new (also a miss); a budget above the read count caps at all four reads.
func TestFillVersionsPriorityAndHitMiss(t *testing.T) {
	t.Parallel()
	// Names distinguish "new" (never has a version, mirroring a freshly-introduced key) from "back"
	// (a back-reference the store already knows a committed version for).
	const roNew, roBack, rwNew, rwBack = "ro-new", "ro-back", "rw-new", "rw-back"
	roKeys := [][]byte{[]byte(roNew), []byte(roBack)}
	rwKeys := [][]byte{[]byte(rwNew), []byte(rwBack)}
	storeVersions := map[string]uint64{roBack: 10, rwBack: 20}

	for _, tc := range []struct {
		name          string
		rate          float64 // single FillVersions call on a fresh accumulator, so k == int(rate).
		wantQueried   []string
		wantVersioned map[string]uint64
	}{
		{
			name:          "budget 1 selects the read-only back-reference first",
			rate:          1,
			wantQueried:   []string{roBack},
			wantVersioned: map[string]uint64{roBack: 10},
		},
		{
			name:          "budget 2 exhausts read-only before read-write",
			rate:          2,
			wantQueried:   []string{roBack, roNew},
			wantVersioned: map[string]uint64{roBack: 10}, // roNew is queried but absent from the store.
		},
		{
			name:          "budget 3 spills into the read-write back-reference",
			rate:          3,
			wantQueried:   []string{roBack, roNew, rwBack},
			wantVersioned: map[string]uint64{roBack: 10, rwBack: 20},
		},
		{
			name:          "budget above the read count caps at all queryable reads",
			rate:          100,
			wantQueried:   []string{roBack, roNew, rwBack, rwNew},
			wantVersioned: map[string]uint64{roBack: 10, rwBack: 20},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tx := newTestTx(roKeys, rwKeys)
			stub := &stubKeyVersionQuerier{versions: singleNsVersions(storeVersions)}
			f := newQueryFiller(stub, tc.rate)

			require.NoError(t, f.FillVersions(t.Context(), []*applicationpb.Tx{tx}))

			require.Len(t, stub.calls, 1)
			require.ElementsMatch(t, tc.wantQueried, queriedKeyStrings(stub.calls[0]))
			requireVersions(t, tx, tc.wantVersioned)
		})
	}
}

// TestFillVersionsZeroRateSkipsQuerying covers queries-rate == 0: no read is ever selected, so GetRows is
// never called, even though the store has a version for every key in the transaction.
func TestFillVersionsZeroRateSkipsQuerying(t *testing.T) {
	t.Parallel()
	tx := newTestTx([][]byte{[]byte("ro")}, [][]byte{[]byte("rw")})
	stub := &stubKeyVersionQuerier{versions: singleNsVersions(map[string]uint64{"ro": 1, "rw": 2})}
	f := newQueryFiller(stub, 0)

	require.NoError(t, f.FillVersions(t.Context(), []*applicationpb.Tx{tx}))

	require.Empty(t, stub.calls)
	requireVersions(t, tx, nil)
}

// TestFillVersionsIgnoresBlindWrites proves blind writes are never selected: even with a budget far
// exceeding the transaction's version-bearing reads, and the store knowing the blind write's key too, the
// blind write is untouched and its key never appears in the query.
func TestFillVersionsIgnoresBlindWrites(t *testing.T) {
	t.Parallel()
	tx := newTestTx([][]byte{[]byte("ro")}, [][]byte{[]byte("rw")})
	blindWrite := &applicationpb.Write{Key: []byte("bw"), Value: []byte("v")}
	tx.Namespaces[0].BlindWrites = []*applicationpb.Write{blindWrite}
	stub := &stubKeyVersionQuerier{versions: singleNsVersions(map[string]uint64{"ro": 1, "rw": 2, "bw": 3})}
	f := newQueryFiller(stub, 100)

	require.NoError(t, f.FillVersions(t.Context(), []*applicationpb.Tx{tx}))

	require.Len(t, tx.Namespaces[0].BlindWrites, 1)
	test.RequireProtoEqual(t, blindWrite, tx.Namespaces[0].BlindWrites[0])
	require.NotContains(t, queriedKeyStrings(stub.calls[0]), "bw")
}

// TestFillVersionsGroupsByNamespace proves the querier doesn't hardcode a namespace id: two transactions
// in the same batch use different namespaces (neither is the default generated one), and both happen to
// read the same key string. The query must target both namespaces' real ids, and the returned versions
// must be mapped back scoped to each read's own namespace — proven by giving the store two different
// versions for the same key, one per namespace, so a namespace mix-up would silently produce a wrong
// version rather than an error.
func TestFillVersionsGroupsByNamespace(t *testing.T) {
	t.Parallel()
	const nsA, nsB, sharedKey = "ns-a", "ns-b", "shared"
	txA := newTestTxNs(nsA, [][]byte{[]byte(sharedKey)}, nil)
	txB := newTestTxNs(nsB, [][]byte{[]byte(sharedKey)}, nil)
	stub := &stubKeyVersionQuerier{versions: map[string]map[string]uint64{
		nsA: {sharedKey: 100},
		nsB: {sharedKey: 200},
	}}
	f := newQueryFiller(stub, 1) // budget 1 per tx: a fresh accumulator selects both txs' lone read.

	require.NoError(t, f.FillVersions(t.Context(), []*applicationpb.Tx{txA, txB}))

	require.Len(t, stub.calls, 1)
	require.ElementsMatch(t, []string{nsA, nsB}, queriedNamespaceIDs(stub.calls[0]))
	require.Equal(t, []string{sharedKey}, queriedKeyStringsForNamespace(stub.calls[0], nsA))
	require.Equal(t, []string{sharedKey}, queriedKeyStringsForNamespace(stub.calls[0], nsB))
	requireVersions(t, txA, map[string]uint64{sharedKey: 100})
	requireVersions(t, txB, map[string]uint64{sharedKey: 200})
}

// TestFillVersionsSpendsPerTxBudgetAcrossNamespaces proves a single transaction's per-tx budget is spent
// across all of its namespaces, in namespace order, reverse-priority within each — not confined to
// namespace 0. A budget of 3 exhausts ns-a's two reads (reverse order: a1 then a0) before spilling one
// read into ns-b (b1).
func TestFillVersionsSpendsPerTxBudgetAcrossNamespaces(t *testing.T) {
	t.Parallel()
	const nsA, nsB = "ns-a", "ns-b"
	tx := &applicationpb.Tx{Namespaces: []*applicationpb.TxNamespace{
		newTxNamespace(nsA, [][]byte{[]byte("a0"), []byte("a1")}, nil),
		newTxNamespace(nsB, [][]byte{[]byte("b0"), []byte("b1")}, nil),
	}}
	stub := &stubKeyVersionQuerier{versions: map[string]map[string]uint64{
		nsA: {"a1": 1},
		nsB: {"b1": 2},
	}}
	f := newQueryFiller(stub, 3)

	require.NoError(t, f.FillVersions(t.Context(), []*applicationpb.Tx{tx}))

	require.ElementsMatch(t, []string{"a1", "a0"}, queriedKeyStringsForNamespace(stub.calls[0], nsA))
	require.Equal(t, []string{"b1"}, queriedKeyStringsForNamespace(stub.calls[0], nsB))
	requireVersions(t, tx, map[string]uint64{"a1": 1, "b1": 2})
}

// TestFillVersionsToleratesMissingNamespace proves the loadgen side of the two-part contract with the
// query service: a NotFound from GetRows — the query service's signal that a selected namespace does not
// exist — is tolerated. FillVersions returns no error and leaves the selected reads at their nil version.
// Any other error still fails the batch, so a real failure is not silently swallowed.
func TestFillVersionsToleratesMissingNamespace(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		err       error
		wantError bool
	}{
		{
			name: "NotFound is tolerated as an all-miss",
			err:  status.Error(codes.NotFound, `namespace does not exist: relation "ns_0" does not exist`),
		},
		{
			name:      "any other error still fails the batch",
			err:       status.Error(codes.Internal, "boom"),
			wantError: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tx := newTestTx([][]byte{[]byte("ro")}, [][]byte{[]byte("rw")})
			stub := &stubKeyVersionQuerier{err: tc.err}
			f := newQueryFiller(stub, 100) // budget large enough to select every read, forcing a GetRows call.

			err := f.FillVersions(t.Context(), []*applicationpb.Tx{tx})

			require.Len(t, stub.calls, 1, "a non-empty selection must still issue the GetRows call")
			if tc.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			requireVersions(t, tx, nil) // either way, no version was filled in — the reads stay nil.
		})
	}
}

// stubKeyVersionQuerier is a keyVersionQuerier test double, keyed like the real query service: NsId, then
// key. GetRows returns a Row (a hit) for every requested key present under its namespace in versions, and
// silently omits (a miss, matching the query service's sparse response) any requested key absent from it
// — including a key requested under a namespace the store doesn't know at all. Every call is recorded for
// assertions on what was queried.
type stubKeyVersionQuerier struct {
	versions map[string]map[string]uint64 // NsId -> key -> version.
	err      error                        // when non-nil, GetRows fails with it instead of returning rows.
	calls    []*committerpb.Query
}

// GetRows implements keyVersionQuerier.
func (s *stubKeyVersionQuerier) GetRows(
	_ context.Context, in *committerpb.Query, _ ...grpc.CallOption,
) (*committerpb.Rows, error) {
	s.calls = append(s.calls, in)
	if s.err != nil {
		return nil, s.err
	}
	namespaces := make([]*committerpb.RowsNamespace, 0, len(in.Namespaces))
	for _, ns := range in.Namespaces {
		nsVersions := s.versions[ns.NsId]
		var rows []*committerpb.Row
		for _, key := range ns.Keys {
			if v, ok := nsVersions[string(key)]; ok {
				rows = append(rows, &committerpb.Row{Key: key, Version: v})
			}
		}
		namespaces = append(namespaces, &committerpb.RowsNamespace{NsId: ns.NsId, Rows: rows})
	}
	return &committerpb.Rows{Namespaces: namespaces}, nil
}

// singleNsVersions wraps a flat key -> version store under the default generated namespace, for tests that
// only care about a single namespace.
func singleNsVersions(versions map[string]uint64) map[string]map[string]uint64 {
	return map[string]map[string]uint64{DefaultGeneratedNamespaceID: versions}
}

// newTxNamespace builds a TxNamespace from explicit read-only and read-write keys, each starting with a
// nil version — the shape queryFiller.FillVersions expects to fill in, in place.
func newTxNamespace(nsID string, roKeys, rwKeys [][]byte) *applicationpb.TxNamespace {
	ro := make([]*applicationpb.Read, len(roKeys))
	for i, key := range roKeys {
		ro[i] = &applicationpb.Read{Key: key}
	}
	rw := make([]*applicationpb.ReadWrite, len(rwKeys))
	for i, key := range rwKeys {
		rw[i] = &applicationpb.ReadWrite{Key: key}
	}
	return &applicationpb.TxNamespace{NsId: nsID, ReadsOnly: ro, ReadWrites: rw}
}

// newTestTxNs builds a single-namespace Tx under nsID from explicit read-only and read-write keys.
func newTestTxNs(nsID string, roKeys, rwKeys [][]byte) *applicationpb.Tx {
	return &applicationpb.Tx{Namespaces: []*applicationpb.TxNamespace{newTxNamespace(nsID, roKeys, rwKeys)}}
}

// newTestTx builds a single-namespace Tx under the default generated namespace.
func newTestTx(roKeys, rwKeys [][]byte) *applicationpb.Tx {
	return newTestTxNs(DefaultGeneratedNamespaceID, roKeys, rwKeys)
}

// queriedKeyStrings flattens a Query's keys (across all its namespaces) into strings, for
// order-independent assertions that don't care which namespace a key was queried under.
func queriedKeyStrings(q *committerpb.Query) []string {
	var keys []string
	for _, ns := range q.Namespaces {
		for _, key := range ns.Keys {
			keys = append(keys, string(key))
		}
	}
	return keys
}

// queriedNamespaceIDs returns the NsId of every QueryNamespace in q, for asserting which namespaces were
// actually targeted.
func queriedNamespaceIDs(q *committerpb.Query) []string {
	ids := make([]string, len(q.Namespaces))
	for i, ns := range q.Namespaces {
		ids[i] = ns.NsId
	}
	return ids
}

// queriedKeyStringsForNamespace returns the keys queried under nsID specifically, or nil if q has no
// QueryNamespace for it.
func queriedKeyStringsForNamespace(q *committerpb.Query, nsID string) []string {
	for _, ns := range q.Namespaces {
		if ns.NsId == nsID {
			return queriedKeyStrings(&committerpb.Query{Namespaces: []*committerpb.QueryNamespace{ns}})
		}
	}
	return nil
}

// requireVersions asserts, for every read-only and read-write entry across all of tx's namespaces, that
// its version was filled in from want when its key is present there, and stayed nil otherwise (either
// because it was never selected, or selected but absent from the store). want is keyed by plain key only,
// so callers with keys that repeat across namespaces (e.g. testing namespace scoping) must call this once
// per namespace/tx rather than across a mixed batch.
func requireVersions(t *testing.T, tx *applicationpb.Tx, want map[string]uint64) {
	t.Helper()
	for _, ns := range tx.Namespaces {
		for _, r := range ns.ReadsOnly {
			requireVersion(t, r.Key, r.Version, want)
		}
		for _, rw := range ns.ReadWrites {
			requireVersion(t, rw.Key, rw.Version, want)
		}
	}
}

func requireVersion(t *testing.T, key []byte, got *uint64, want map[string]uint64) {
	t.Helper()
	v, ok := want[string(key)]
	if !ok {
		require.Nil(t, got, "key %q should not have a version", key)
		return
	}
	require.NotNil(t, got, "key %q should have a version", key)
	require.Equal(t, v, *got, "key %q version mismatch", key)
}

// countVersioned counts tx's read-only and read-write entries, across all namespaces, that got a non-nil
// version.
func countVersioned(tx *applicationpb.Tx) int {
	count := 0
	for _, ns := range tx.Namespaces {
		for _, r := range ns.ReadsOnly {
			if r.Version != nil {
				count++
			}
		}
		for _, rw := range ns.ReadWrites {
			if rw.Version != nil {
				count++
			}
		}
	}
	return count
}
