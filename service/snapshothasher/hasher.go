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
	"strings"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/yugabyte/pgx/v5"
	"github.com/yugabyte/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"github.com/hyperledger/fabric-x-committer/utils/retry"
	"github.com/hyperledger/fabric-x-committer/utils/statedb"
)

const (
	// txStatusPageSQL pages tx_status in primary-key order for hashing. tx_id is the
	// PRIMARY KEY of tx_status, so ORDER BY tx_id is an index-order scan with no sort
	// step, and `tx_id > $1` is an index seek.
	txStatusPageSQL = "SELECT tx_id, status, height FROM tx_status WHERE tx_id > $1 ORDER BY tx_id LIMIT $2"

	// nsRowPageSQLTempl pages an ns_<id> table in primary-key order for hashing.
	// `key` is the PRIMARY KEY of ns_<id>, so ORDER BY key is served directly from the
	// primary-key index (index-order scan) — there is no sort step, and the keyset
	// predicate `key > $1` is an index seek. ${TABLE} is a sanitized identifier built
	// from ns__meta keys, not user input.
	nsRowPageSQLTempl = "SELECT key, value FROM ${TABLE} WHERE key > $1 ORDER BY key LIMIT $2"
)

// hasher computes the deterministic content hash of a snapshot clone database. It
// holds only what hashing needs -- read-only connection config, resource limits,
// and a retry profile -- so it can run against any clone without touching the
// scheduler's state.
type hasher struct {
	config         *statedb.Config
	resourceLimits *ResourceLimitsConfig
	retryProfile   *retry.Profile
}

func newHasher(config *Config) *hasher {
	return &hasher{
		config:         config.Database,
		resourceLimits: config.ResourceLimits,
		retryProfile:   config.Database.Retry,
	}
}

// hashSnapshotDatabase opens a short-lived pool on the clone database, hashes
// every hashed table in parallel, and combines the per-table digests in sorted
// table-name order into one deterministic SHA-256.
//
// Hashed set (derived from ns__meta, the authoritative namespace registry):
// every user namespace's ns_<id> table, plus ns__meta, ns__config, and
// tx_status. metadata, ns__snapshot, and ns__checkpoint are excluded. The
// result is identical for identical clone content regardless of table-
// completion order, because each table is hashed independently and the combine
// step re-sorts by table name.
func (h *hasher) hashSnapshotDatabase(ctx context.Context, cloneDatabase string) ([]byte, error) {
	pool, err := h.openClonePool(ctx, cloneDatabase)
	if err != nil {
		return nil, err
	}
	defer pool.Close()

	tables, err := listHashedTables(ctx, pool, h.retryProfile)
	if err != nil {
		return nil, err
	}

	cfg := tableHashConfig{
		pool: pool, batchSize: h.resourceLimits.HashBatchSize, retryProfile: h.retryProfile,
	}
	tableHashes := make([][]byte, len(tables))

	g, gCtx := errgroup.WithContext(ctx)
	g.SetLimit(h.resourceLimits.MaxWorkersForHash)

	for i, table := range tables {
		g.Go(func() error {
			hh, hErr := hashTable(gCtx, cfg, table)
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

	// Combine in sorted table-name order (tables is already sorted).
	//
	// NOTE (future work, phase 2): only the combined root hash is persisted today.
	// To localize a divergence between organizations we must also preserve the
	// per-table hashes, so two orgs can compare table-by-table and identify which
	// table disagrees. Narrowing the diff *within* that table then requires a
	// Merkle tree over its rows. Both are deferred to phase 2 and do not change
	// the root-hash encoding computed here.
	final := sha256.New()
	for i, table := range tables {
		writeLengthPrefixed(final, []byte(table))
		writeLengthPrefixed(final, tableHashes[i])
	}
	return final.Sum(nil), nil
}

// openClonePool opens a short-lived pgxpool against the clone database, sized to
// exactly the per-table worker count. A worker holds a connection for as long as it
// is reading a page of its table, so at most one connection per worker is in use at
// any instant: a smaller pool would serialize workers that SetLimit says may run in
// parallel, and a larger one would only hold connections nothing uses. No connection
// is reserved for listHashedTables, because it completes before the first worker
// starts and has already returned its connection to the pool.
//
// The pool deliberately ignores the configured database.max-connections and
// min-connections: those size the long-lived pool that polls the `_snapshot` record on
// the source database, which is a different database and a one-statement-at-a-time
// workload. Nothing warms this pool either, since it is opened per hash job and closed
// when the job ends.
//
// An absent clone fails here immediately rather than after the retry budget, because
// statedb.NewPool treats a missing database as terminal, and is reported as
// ErrCorruptSnapshotState: the clone is created before its snapshot transaction
// commits, so a committed record naming a clone that does not exist is an invariant
// violation to surface, not a hash failure to retry.
func (h *hasher) openClonePool(ctx context.Context, cloneDatabase string) (*pgxpool.Pool, error) {
	cfg := *h.config
	cfg.Database = cloneDatabase
	//nolint:gosec // small bounded worker count.
	cfg.MaxConnections = int32(h.resourceLimits.MaxWorkersForHash)
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
// ns__snapshot and ns__checkpoint are never in ns__meta and are not added, so
// they are naturally excluded.
func listHashedTables(ctx context.Context, pool *pgxpool.Pool, retryProfile *retry.Profile) ([]string, error) {
	// metaTable is a sanitized fixed identifier, not user input.
	metaTable := pgx.Identifier{statedb.TableName(committerpb.MetaNamespaceID)}.Sanitize()
	metaRows, err := retry.ExecuteWithResult(ctx, retryProfile, func() ([]struct{ Key []byte }, error) {
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

// tableHashConfig bundles the connection and tuning knobs shared by every table
// hash in one hashSnapshotDatabase call, keeping hashTable/hashPaginatedTable under
// the linter's argument-count limit despite needing pool, batchSize, and
// retryProfile.
type tableHashConfig struct {
	pool         *pgxpool.Pool
	batchSize    int
	retryProfile *retry.Profile
}

// hashTable scans one table in primary-key order in bounded pages (keyset
// pagination) and folds rows into a per-table SHA-256 using length-prefixed
// encoding len(key)||key||len(value)||value. tx_status is encoded as key=tx_id,
// value=int32BE(status)||height. Paging bounds worker memory on large tables;
// ORDER BY the primary key is an index-order scan (no sort step).
func hashTable(ctx context.Context, cfg tableHashConfig, table string) ([]byte, error) {
	if table == statedb.TxStatusTableName {
		return hashPaginatedTable[txStatusRow](ctx, cfg, txStatusPageSQL, statedb.TxStatusTableName)
	}
	// table is a sanitized identifier built from ns__meta keys, not user input.
	sanitizedTable := pgx.Identifier{table}.Sanitize()
	q := strings.ReplaceAll(nsRowPageSQLTempl, "${TABLE}", sanitizedTable)
	return hashPaginatedTable[nsRow](ctx, cfg, q, sanitizedTable)
}

// hashPaginatedTable hashes a table in keyset-paginated pages, shared by both
// branches of hashTable (ns_<id> and tx_status): it queries a page (retried),
// folds each row's pageRow.hashKV() into a running SHA-256, and re-issues the
// query with the last row's pagingKey() as the next page's lower bound.
//
// NOTE (future work): fetching and hashing are sequential here — each page waits
// for the previous hash fold and vice versa. They could be pipelined into two
// goroutines (fetch page N+1 while hashing page N). We deliberately do not, to
// avoid driving extra concurrent read load against a cluster that is also
// serving live transactions. If pipelining is added later, consider a
// configurable per-page delay to cap the read rate.
func hashPaginatedTable[T pageRow](
	ctx context.Context, cfg tableHashConfig, query, tableNameForErr string,
) ([]byte, error) {
	h := sha256.New()
	// keys/tx_ids are always non-empty in this system, so the empty-bytes lower bound
	// includes the first real row (empty BYTEA sorts below every non-empty key). A
	// genuinely empty key would be skipped by `key > $1` (`'' > ''` is false), which is
	// acceptable given the non-empty invariant.
	lastKey := []byte{}
	for {
		// Re-issuing the query per page is cheap: the keyset predicate is an index seek.
		page, err := retry.ExecuteWithResult(ctx, cfg.retryProfile, func() ([]T, error) {
			rows, queryErr := cfg.pool.Query(ctx, query, lastKey, cfg.batchSize)
			if queryErr != nil {
				return nil, errors.Wrapf(queryErr, "failed to query page of table %s", tableNameForErr)
			}
			defer rows.Close()
			collected, collectErr := pgx.CollectRows(rows, pgx.RowToStructByPos[T])
			return collected, errors.Wrapf(collectErr, "failed to collect page of table %s", tableNameForErr)
		})
		if err != nil {
			return nil, err
		}

		for i := range page {
			key, value := page[i].hashKV()
			writeLengthPrefixed(h, key)
			writeLengthPrefixed(h, value)
		}
		if len(page) < cfg.batchSize {
			break
		}
		lastKey = page[len(page)-1].pagingKey()
	}
	return h.Sum(nil), nil
}

// pageRow is the shared shape hashPaginatedTable needs from a table row: a
// keyset-pagination cursor and a key/value pair to fold into the table hash.
// Implemented by nsRow and txStatusRow so hashTable's ns_<id> and tx_status
// branches can share one paging/hashing skeleton despite their different SQL
// and columns.
type pageRow interface {
	pagingKey() []byte
	hashKV() ([]byte, []byte)
}

// nsRow is one ns_<id> table row, collected positionally (SELECT key, value).
type nsRow struct {
	Key   []byte
	Value []byte
}

// pagingKey returns the keyset-pagination cursor value for this row.
func (r nsRow) pagingKey() []byte {
	return r.Key
}

// hashKV returns the length-prefix-encoded key/value pair folded into the table hash.
func (r nsRow) hashKV() (key, value []byte) {
	return r.Key, r.Value
}

// txStatusRow is one tx_status row, collected positionally (SELECT tx_id, status, height).
type txStatusRow struct {
	TxID   []byte
	Status int32
	Height []byte
}

// pagingKey returns the keyset-pagination cursor value for this row.
func (r txStatusRow) pagingKey() []byte {
	return r.TxID
}

// hashKV returns key=tx_id, value=int32BE(status)||height, folded into the table hash.
func (r txStatusRow) hashKV() (key, value []byte) {
	value = make([]byte, 4, 4+len(r.Height))
	binary.BigEndian.PutUint32(value, uint32(r.Status)) //nolint:gosec // status is a small enum.
	value = append(value, r.Height...)
	return r.TxID, value
}

// writeLengthPrefixed writes an 8-byte big-endian length followed by the bytes.
// The length prefix prevents boundary collisions (e.g. "ab"+"cd" vs "abc"+"d").
func writeLengthPrefixed(h io.Writer, b []byte) {
	var lenBuf [8]byte
	binary.BigEndian.PutUint64(lenBuf[:], uint64(len(b)))
	_, _ = h.Write(lenBuf[:]) // sha256 Write never errors.
	_, _ = h.Write(b)
}
