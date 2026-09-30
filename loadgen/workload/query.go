/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package workload

import (
	"context"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"

	"github.com/hyperledger/fabric-x-committer/utils/grpcerror"
)

type (
	// queryFiller is the concrete batchQuerier: it queries the committed version of a rate-controlled
	// subset of each batch's reads and fills them in before signing.
	//
	// acc is a persistent fractional accumulator, NOT reset between FillVersions calls — a queryFiller is
	// used by a single worker goroutine, so it needs no synchronization. This is what makes rate an
	// accurate AVERAGE for any batch size, critically including a batch size of 1: with a per-call
	// accumulator instead, a rate below 1 would floor to zero queries on every single-tx batch.
	queryFiller struct {
		client keyVersionQuerier
		rate   float64
		acc    float64
	}

	// keyVersionQuerier is the minimal seam a queryFiller needs to fetch committed key versions. It is
	// satisfied by committerpb.QueryServiceClient — a later task dials the real client and injects it, one
	// per worker; tests inject a stub.
	keyVersionQuerier interface {
		GetRows(ctx context.Context, in *committerpb.Query, opts ...grpc.CallOption) (*committerpb.Rows, error)
	}

	// nsSelection is the subset of a single namespace's version-bearing reads selected for querying, across
	// the whole batch.
	nsSelection struct {
		reads      []*applicationpb.Read
		readWrites []*applicationpb.ReadWrite
	}
)

// newQueryFiller creates a batchQuerier that queries client for a rate-controlled average number of reads
// per transaction.
func newQueryFiller(client keyVersionQuerier, rate float64) *queryFiller {
	return &queryFiller{client: client, rate: rate}
}

// FillVersions selects a rate-controlled subset of the batch's version-bearing reads (ReadsOnly and
// ReadWrites; blind writes carry no version field), grouped by their own transaction namespace's NsId
// (a transaction may span more than one namespace), fetches their committed versions with a single
// GetRows call across the whole batch — one QueryNamespace per distinct NsId actually selected — and fills
// them in, in place. A selected read whose key comes back absent from the query service keeps its nil
// version (a miss — the key has no committed version yet).
func (f *queryFiller) FillVersions(ctx context.Context, batch []*applicationpb.Tx) error {
	selected := make(map[string]*nsSelection)
	for _, tx := range batch {
		// Fractional accumulator: add this tx's share of the rate, take the whole part as this tx's
		// query count, and subtract it back out so acc keeps only the fractional remainder, carried
		// into the next tx. Over many txs the per-tx counts average out to rate — e.g. rate 0.3 means
		// a query on ~3 of every 10 txs — no matter how small each individual batch is.
		f.acc += f.rate
		k := int(f.acc)
		f.acc -= float64(k)
		selectPriorityReads(tx, k, selected)
	}
	if len(selected) == 0 {
		return nil
	}

	rows, err := f.client.GetRows(ctx, buildQuery(selected))
	if err != nil {
		// NotFound means a selected namespace does not exist. Every selected key in it is therefore a
		// miss, which is exactly the nil version the reads already carry — so leave them untouched
		// rather than failing the batch.
		if grpcerror.HasCode(err, codes.NotFound) {
			return nil
		}
		return errors.Wrap(err, "failed to query committed key versions")
	}

	fillSelectedVersions(selected, versionsByNamespace(rows))
	return nil
}

// fillSelectedVersions writes each fetched committed version back onto its selected read, in place, scoped
// to the read's own namespace. A selected key absent from versions (a miss) keeps its nil version.
func fillSelectedVersions(selected map[string]*nsSelection, versions map[string]map[string]uint64) {
	for nsID, sel := range selected {
		nsVersions := versions[nsID]
		for _, r := range sel.reads {
			if v, ok := nsVersions[string(r.Key)]; ok {
				r.Version = &v
			}
		}
		for _, rw := range sel.readWrites {
			if v, ok := nsVersions[string(rw.Key)]; ok {
				rw.Version = &v
			}
		}
	}
}

// selectPriorityReads selects up to k of tx's version-bearing reads (ReadsOnly and ReadWrites; blind
// writes have no version field, so they are never candidates) and buckets them into selected, keyed by
// each read's own namespace's NsId. The budget k is spent across tx's namespaces in order; within each
// namespace, in the querier's priority order: ReadsOnly from last to first, then ReadWrites from last to
// first. Reads are laid out new-keys-first (see tx_rand.go's splitSlotKeys), so this reverse order reaches
// back-references — which may already carry a committed version — before newly-introduced keys, which
// cannot have one yet.
func selectPriorityReads(tx *applicationpb.Tx, k int, selected map[string]*nsSelection) {
	for _, ns := range tx.GetNamespaces() {
		if k <= 0 {
			return
		}
		readsOnly := ns.GetReadsOnly()
		readWrites := ns.GetReadWrites()
		if len(readsOnly) == 0 && len(readWrites) == 0 {
			continue
		}
		sel := selected[ns.GetNsId()]
		if sel == nil {
			sel = &nsSelection{}
			selected[ns.GetNsId()] = sel
		}
		for i := len(readsOnly) - 1; i >= 0 && k > 0; i-- {
			sel.reads = append(sel.reads, readsOnly[i])
			k--
		}
		for i := len(readWrites) - 1; i >= 0 && k > 0; i-- {
			sel.readWrites = append(sel.readWrites, readWrites[i])
			k--
		}
	}
}

// buildQuery builds a single Query with one QueryNamespace per distinct namespace in selected, each with
// its deduped keys, for a single batch-wide GetRows call.
func buildQuery(selected map[string]*nsSelection) *committerpb.Query {
	namespaces := make([]*committerpb.QueryNamespace, 0, len(selected))
	for nsID, sel := range selected {
		namespaces = append(namespaces, &committerpb.QueryNamespace{
			NsId: nsID,
			Keys: dedupKeys(sel.reads, sel.readWrites),
		})
	}
	return &committerpb.Query{Namespaces: namespaces}
}

// dedupKeys collects the distinct keys across reads and readWrites, for a single namespace's query.
func dedupKeys(reads []*applicationpb.Read, readWrites []*applicationpb.ReadWrite) [][]byte {
	seen := make(map[string]struct{}, len(reads)+len(readWrites))
	keys := make([][]byte, 0, len(reads)+len(readWrites))
	add := func(key []byte) {
		if _, ok := seen[string(key)]; ok {
			return
		}
		seen[string(key)] = struct{}{}
		keys = append(keys, key)
	}
	for _, r := range reads {
		add(r.Key)
	}
	for _, rw := range readWrites {
		add(rw.Key)
	}
	return keys
}

// versionsByNamespace extracts the sparse NsId -> key -> version map from a GetRows response. A key with
// no committed version yet simply has no entry within its namespace (a miss).
func versionsByNamespace(rows *committerpb.Rows) map[string]map[string]uint64 {
	namespaces := rows.GetNamespaces()
	versions := make(map[string]map[string]uint64, len(namespaces))
	for _, ns := range namespaces {
		rs := ns.GetRows()
		nsVersions := make(map[string]uint64, len(rs))
		for _, row := range rs {
			nsVersions[string(row.GetKey())] = row.GetVersion()
		}
		versions[ns.GetNsId()] = nsVersions
	}
	return versions
}
