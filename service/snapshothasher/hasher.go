/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package snapshothasher

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"slices"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/yugabyte/pgx/v5"
	"github.com/yugabyte/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"github.com/hyperledger/fabric-x-committer/utils/retry"
	"github.com/hyperledger/fabric-x-committer/utils/statedb"
)

// txStatusPageSQL pages tx_status in primary-key order for hashing, encoding the
// hashed value in SQL as int32BE(status)||height so every table yields the same
// (key, value) row shape. tx_id is the PRIMARY KEY, so ORDER BY tx_id is an
// index-order scan with no sort step, and `tx_id > $1` is an index seek.
//
// status is a nullable column, and NULL || height is NULL, not height: without the
// coalesce a single NULL status would collapse the whole concatenation and drop that
// row's height from the digest, so two rows differing only in height would hash
// identically. The sentinel is negative, which no committerpb.Status value is, so it
// cannot collide with a real status.
const txStatusPageSQL = "SELECT tx_id, int4send(coalesce(status, -1)) || height FROM tx_status " +
	"WHERE tx_id > $1 ORDER BY tx_id LIMIT $2"

// hasher computes the deterministic content hash of a snapshot clone database.
type hasher struct {
	config *Config
}

func newHasher(config *Config) *hasher {
	return &hasher{config: config}
}

// hashSnapshotDatabase opens a short-lived pool on the clone database, hashes
// every hashed table in parallel, and combines the per-table digests in sorted
// table-name order into one deterministic SHA-256. The caller opens the pool (see
// openClonePool) so that a clone that is not there to hash is detected before any
// state is written for the attempt.
//
// Hashed set (derived from ns__meta, the authoritative namespace registry):
// every user namespace's ns_<id> table, plus ns__meta, ns__config, and
// tx_status. metadata, ns__snapshot, and ns__checkpoint are excluded.
func (h *hasher) hashSnapshotDatabase(ctx context.Context, pool *pgxpool.Pool) ([]byte, error) {
	tables, err := h.listHashedTables(ctx, pool)
	if err != nil {
		return nil, err
	}
	tableHashes := make([][]byte, len(tables))

	g, gCtx := errgroup.WithContext(ctx)
	g.SetLimit(h.config.ResourceLimits.MaxWorkersForHash)

	for i, table := range tables {
		g.Go(func() error {
			hh, hErr := h.hashTable(gCtx, pool, table)
			if hErr != nil {
				return fmt.Errorf("failed to hash table %s: %w", table, hErr)
			}
			tableHashes[i] = hh
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}

	// Combine in sorted table-name order (tables is already sorted), so the digest
	// does not depend on table-completion order.
	//
	// NOTE (future work, phase 2): only the combined root hash is persisted today.
	// To localize a divergence between organizations we must also preserve the
	// per-table hashes, plus a Merkle tree over a table's rows to narrow the diff
	// within it. Neither changes the root-hash encoding computed here.
	final := sha256.New()
	for i, table := range tables {
		writeLengthPrefixed(final, []byte(table))
		writeLengthPrefixed(final, tableHashes[i])
	}
	return final.Sum(nil), nil
}

// openClonePool opens a short-lived pgxpool against the clone database, sized to
// exactly the per-table worker count: a worker holds a connection only while
// reading a page, and listHashedTables has returned its connection before the
// first worker starts. The configured max/min-connections size the long-lived
// pool on the SOURCE database, which is a different database and a
// one-statement-at-a-time workload, so they are deliberately ignored here.
//
// A clone is created before its snapshot transaction commits, so a committed
// record naming a clone that does not exist is an invariant violation: it is
// reported as ErrCorruptSnapshotState, and fails immediately because
// statedb.NewPool treats a missing database as terminal.
func (h *hasher) openClonePool(ctx context.Context, cloneDatabase string) (*pgxpool.Pool, error) {
	cfg := *h.config.Database
	cfg.Database = cloneDatabase
	//nolint:gosec // small bounded worker count.
	cfg.MaxConnections = int32(h.config.ResourceLimits.MaxWorkersForHash)
	cfg.MinConnections = 0

	pool, err := statedb.NewPool(ctx, &cfg)
	if errors.Is(err, statedb.ErrDatabaseNotFound) {
		return nil, errors.Wrapf(errors.Join(ErrCorruptSnapshotState, err),
			"snapshot clone %s does not exist", cloneDatabase)
	}
	if err != nil {
		return nil, errors.Wrapf(err, "failed to open pool on snapshot clone %s", cloneDatabase)
	}
	return pool, nil
}

// listHashedTables returns the sorted list of table names to hash on the clone.
// It reads the namespace registry from ns__meta (one key per user namespace),
// then appends the fixed system tables ns__meta, ns__config, and tx_status.
// ns__snapshot and ns__checkpoint are never in ns__meta, so they are naturally
// excluded.
func (h *hasher) listHashedTables(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	// metaTable is a sanitized fixed identifier, not user input.
	metaTable := pgx.Identifier{statedb.TableName(committerpb.MetaNamespaceID)}.Sanitize()
	metaRows, err := retry.ExecuteWithResult(ctx, h.config.Database.Retry, func() ([]struct{ Key []byte }, error) {
		rows, queryErr := pool.Query(ctx, fmt.Sprintf("SELECT key FROM %s", metaTable))
		if queryErr != nil {
			return nil, errors.Wrap(queryErr, "failed to read namespace registry from ns__meta")
		}
		defer rows.Close()
		collected, collectErr := pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ Key []byte }])
		return collected, errors.Wrap(collectErr, "failed to collect ns__meta rows")
	})
	if err != nil {
		return nil, err
	}

	tables := make([]string, 0, len(metaRows)+3)
	for i := range metaRows {
		tables = append(tables, statedb.TableName(string(metaRows[i].Key)))
	}
	// Fixed system tables that hold committed state but are not registered in ns__meta.
	tables = append(
		tables,
		statedb.TableName(committerpb.MetaNamespaceID),
		statedb.TableName(committerpb.ConfigNamespaceID),
		statedb.TxStatusTableName,
	)
	slices.Sort(tables)
	return tables, nil
}

// hashTable scans one table in primary-key order in bounded pages (keyset
// pagination) and folds rows into a per-table SHA-256 using length-prefixed
// encoding len(key)||key||len(value)||value. tx_status is encoded as key=tx_id,
// value=int32BE(status)||height (see txStatusPageSQL). Paging bounds worker
// memory on large tables; ORDER BY the primary key is an index-order scan.
//
// NOTE (future work): fetching and hashing are sequential -- each page waits for
// the previous hash fold and vice versa. Pipelining them (fetch page N+1 while
// hashing page N) is deliberately not done, to avoid extra concurrent read load
// on a cluster that is also serving live transactions.
func (h *hasher) hashTable(ctx context.Context, pool *pgxpool.Pool, table string) ([]byte, error) {
	// table is a sanitized identifier built from ns__meta keys, not user input.
	sanitizedTable := pgx.Identifier{table}.Sanitize()
	query := txStatusPageSQL
	if table != statedb.TxStatusTableName {
		query = fmt.Sprintf("SELECT key, value FROM %s WHERE key > $1 ORDER BY key LIMIT $2", sanitizedTable)
	}

	batchSize := h.config.ResourceLimits.HashBatchSize
	tableHash := sha256.New()
	// keys/tx_ids are always non-empty in this system, so the empty-bytes lower bound
	// includes the first real row (empty BYTEA sorts below every non-empty key). A
	// genuinely empty key would be skipped by `key > $1` (`'' > ''` is false), which is
	// acceptable given the non-empty invariant.
	lastKey := []byte{}
	for {
		// Re-issuing the query per page is cheap: the keyset predicate is an index seek.
		page, err := retry.ExecuteWithResult(ctx, h.config.Database.Retry, func() ([]row, error) {
			rows, queryErr := pool.Query(ctx, query, lastKey, batchSize)
			if queryErr != nil {
				return nil, errors.Wrapf(queryErr, "failed to query page of table %s", sanitizedTable)
			}
			defer rows.Close()
			collected, collectErr := pgx.CollectRows(rows, pgx.RowToStructByPos[row])
			return collected, errors.Wrapf(collectErr, "failed to collect page of table %s", sanitizedTable)
		})
		if err != nil {
			return nil, err
		}

		for i := range page {
			// A NULL value scans as nil and is hashed as an empty value. That is the storage
			// semantics this system already has -- a write carries proto3 bytes, which cannot
			// distinguish nil from empty -- so the two are the same committed state, not a
			// collision. tx_status cannot reach here with a nil value: see txStatusPageSQL.
			writeLengthPrefixed(tableHash, page[i].Key)
			writeLengthPrefixed(tableHash, page[i].Value)
		}
		if len(page) < batchSize {
			break
		}
		lastKey = page[len(page)-1].Key
	}
	return tableHash.Sum(nil), nil
}

// row is one hashed row, collected positionally: the primary key (also the
// keyset-pagination cursor) and the value folded into the table hash.
type row struct {
	Key   []byte
	Value []byte
}

// writeLengthPrefixed writes an 8-byte big-endian length followed by the bytes.
// The length prefix prevents boundary collisions (e.g. "ab"+"cd" vs "abc"+"d").
func writeLengthPrefixed(h io.Writer, b []byte) {
	var lenBuf [8]byte
	binary.BigEndian.PutUint64(lenBuf[:], uint64(len(b)))
	_, _ = h.Write(lenBuf[:]) // sha256 Write never errors.
	_, _ = h.Write(b)
}
