/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package vc

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

	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/retry"
	"github.com/hyperledger/fabric-x-committer/utils/statedb"
)

// snapshotHashJobBuffer bounds the snapshotHashJobs channel. The sidecar drains around a
// snapshot TX, so at most one snapshot is ever awaiting hashing, and a single outer
// worker goroutine hashes one snapshot at a time; a 1-slot buffer therefore suffices
// to let the committer enqueue without blocking. MaxWorkersForSnapshotHash
// parallelizes the per-table hashes *within* one snapshot job, not across jobs.
const (
	snapshotHashJobBuffer = 1

	updateSnapshotRowSQL = "UPDATE ns_" + committerpb.SnapshotNamespaceID +
		" SET value = $2, version = version + 1 WHERE key = $1;"

	// selectSnapshotRowForUpdateSQL locks the _snapshot row for the duration of the
	// enclosing transaction. beginTx runs at READ COMMITTED, which does not itself
	// serialize concurrent access to a row or fail our commit if another transaction
	// changed it after we read it: our later UPDATE is a blind write keyed only on
	// `key`, so at plain READ COMMITTED a concurrent writer could commit between our
	// SELECT and UPDATE and we would still match and overwrite it, succeeding with a
	// stale value (TOCTOU). FOR UPDATE closes that gap by blocking any concurrent
	// writer on this row until we commit or roll back, so no stale read can survive
	// into our write.
	selectSnapshotRowForUpdateSQL = "SELECT value FROM ns_" + committerpb.SnapshotNamespaceID +
		" WHERE key = $1 FOR UPDATE;"

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

// snapshotHashJob is a background request to hash a snapshot clone database and
// advance its _snapshot record to COMPLETED.
type snapshotHashJob struct {
	cloneDatabase string
	ref           *committerpb.TxRef
}

// snapshotHashJobFromWrites returns the hash job implied by a just-committed batch,
// if any. The preparer synthesizes exactly one _snapshot write per snapshot TX, so
// there is at most one record to inspect; a record not left PENDING by
// createSnapshotDatabaseAndRewriteRecord is a resubmission/duplicate write and must
// not enqueue a second hash job for an already-tracked snapshot.
//
// TODO: PENDING-only excludes IN_PROGRESS/COMPLETED rows on purpose — recovery
// re-enqueuing from a re-read of storage (not from newWrites) is PR-13's scope; widen
// this check only alongside that re-read path.
func snapshotHashJobFromWrites(newWrites transactionToWrites) (snapshotHashJob, bool) {
	w, ok := snapshotWriteInBatch(newWrites)
	if !ok {
		return snapshotHashJob{}, false
	}

	state, err := decodeSnapshotState(w.values[0])
	if err != nil {
		logger.Errorf("failed to decode committed _snapshot record for enqueue: %+v", err)
		return snapshotHashJob{}, false
	}

	if state.Status != committerpb.SnapshotState_PENDING {
		return snapshotHashJob{}, false
	}

	return snapshotHashJob{cloneDatabase: state.CloneDatabase, ref: state.TxRef}, true
}

// enqueueSnapshotHashJob offers a hash job to the background worker. Re-enqueuing an
// already-COMPLETED clone is intentional (recomputes and rewrites its state, see
// TestSnapshotHashReEnqueueIsIdempotent), not treated as a duplicate. A canceled
// context means the commit already succeeded but the job was not queued; the caller
// must not treat this as a failed commit.
func (db *database) enqueueSnapshotHashJob(ctx context.Context, job snapshotHashJob) error {
	if channel.NewWriter(ctx, db.snapshotHashJobs).Write(job) {
		return nil
	}
	return ctx.Err()
}

// runSnapshotHashWorker is started as a goroutine by ValidatorCommitterService.Run
// alongside the preparer/validator/committer workers. It serially processes
// snapshot hash jobs until its context ends.
func (db *database) runSnapshotHashWorker(ctx context.Context) error {
	reader := channel.NewReader(ctx, db.snapshotHashJobs)
	for {
		job, ok := reader.Read()
		if !ok {
			return nil
		}
		if err := db.processSnapshotHashJob(ctx, job); err != nil {
			logger.Errorf("snapshot hash job for %s failed: %+v", job.cloneDatabase, err)
		}
	}
}

func (db *database) processSnapshotHashJob(ctx context.Context, job snapshotHashJob) error {
	if err := db.updateSnapshotState(ctx, job.ref, committerpb.SnapshotState_IN_PROGRESS, nil); err != nil {
		return fmt.Errorf("failed to mark snapshot %s IN_PROGRESS: %w", job.cloneDatabase, err)
	}

	digest, err := db.hasher.hashSnapshotDatabase(ctx, job.cloneDatabase)
	if err != nil {
		return fmt.Errorf("failed to hash snapshot %s: %w", job.cloneDatabase, err)
	}

	if err := db.updateSnapshotState(ctx, job.ref, committerpb.SnapshotState_COMPLETED, digest); err != nil {
		return fmt.Errorf("failed to mark snapshot %s COMPLETED: %w", job.cloneDatabase, err)
	}
	return nil
}

// updateSnapshotState rewrites the _snapshot record for ref.TxId with a new status
// (and hash, when digest is non-nil); TxRef and CloneDatabase are preserved because
// the existing record is decoded, mutated, and re-encoded rather than rebuilt.
//
// The read and the write run inside a single DB transaction using SELECT ... FOR
// UPDATE (see selectSnapshotRowForUpdateSQL), not just beginTx's READ COMMITTED
// isolation alone: READ COMMITTED does not detect this conflict or fail our commit,
// because our UPDATE is a blind write keyed only on `key`, not on the value/version
// we read. Without the row lock, a concurrent writer could commit between our SELECT
// and UPDATE, and we would still match and overwrite it with our stale re-encoded
// value (TOCTOU) with no error at any point. FOR UPDATE blocks that concurrent writer
// on this row until we commit or roll back, closing the gap. The whole
// read-decode-mutate-encode-write sequence is retried as one unit so a transient
// failure anywhere in it restarts from a fresh, consistent read.
func (db *database) updateSnapshotState(
	ctx context.Context,
	ref *committerpb.TxRef,
	status committerpb.SnapshotState_Status,
	digest []byte,
) error {
	key := []byte(ref.TxId)

	return retry.Execute(ctx, db.retryProfile, func() error {
		tx, rollBackFunc, err := db.beginTx(ctx)
		if err != nil {
			return err
		}
		defer rollBackFunc()

		var raw []byte
		scanErr := tx.QueryRow(ctx, selectSnapshotRowForUpdateSQL, key).Scan(&raw)
		if scanErr != nil {
			return errors.Wrapf(scanErr, "failed to read _snapshot record for tx %s", ref.TxId)
		}

		state, decErr := decodeSnapshotState(raw)
		if decErr != nil {
			return errors.Wrapf(decErr, "tx %s", ref.TxId)
		}

		state.Status = status
		if digest != nil {
			state.Hash = digest
		}

		newRaw, encErr := encodeSnapshotState(state)
		if encErr != nil {
			return errors.Wrapf(encErr, "tx %s", ref.TxId)
		}

		if _, exErr := tx.Exec(ctx, updateSnapshotRowSQL, key, newRaw); exErr != nil {
			return errors.Wrapf(exErr, "failed to update _snapshot record for tx %s", ref.TxId)
		}

		return errors.Wrapf(tx.Commit(ctx), "failed to commit _snapshot state update for tx %s", ref.TxId)
	})
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

// pageRow is the shared shape hashPaginatedTable needs from a table row: a
// keyset-pagination cursor and a key/value pair to fold into the table hash.
// Implemented by nsRow and txStatusRow so hashTable's ns_<id> and tx_status
// branches can share one paging/hashing skeleton despite their different SQL
// and columns.
type pageRow interface {
	pagingKey() []byte
	hashKV() ([]byte, []byte)
}

// snapshotHasher computes the deterministic content hash of a snapshot clone
// database. It is a standalone utility, not a method set on *database: hashing
// only needs read-only DB connection config, resource limits, and a retry
// profile, not database's pool, metrics, or in-flight commit state. Keeping it
// separate stops database's method surface from growing across every file
// that touches namespace tables (database.go, database_snapshot.go, database_snapshot_hash.go).
type snapshotHasher struct {
	config         *statedb.Config
	resourceLimits *ResourceLimitsConfig
	retryProfile   *retry.Profile
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
func (h *snapshotHasher) hashSnapshotDatabase(ctx context.Context, cloneDatabase string) ([]byte, error) {
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
		pool: pool, batchSize: h.resourceLimits.SnapshotHashBatchSize, retryProfile: h.retryProfile,
	}
	tableHashes := make([][]byte, len(tables))

	g, gCtx := errgroup.WithContext(ctx)
	g.SetLimit(h.resourceLimits.MaxWorkersForSnapshotHash)

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

// openClonePool opens a pgxpool against the clone database, sized to the
// per-table worker count so parallel scans do not starve on connections.
func (h *snapshotHasher) openClonePool(ctx context.Context, cloneDatabase string) (*pgxpool.Pool, error) {
	cfg := *h.config
	cfg.Database = cloneDatabase
	//nolint:gosec // small bounded worker count.
	cfg.MaxConnections = int32(h.resourceLimits.MaxWorkersForSnapshotHash) + 1

	pool, err := statedb.NewPool(ctx, &cfg)
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

// hashTable scans one table in primary-key order in bounded pages (keyset
// pagination) and folds rows into a per-table SHA-256 using length-prefixed
// encoding len(key)||key||len(value)||value. tx_status is encoded as key=tx_id,
// value=int32BE(status)||height. Paging bounds worker memory on large tables;
// tableHashConfig bundles the connection and tuning knobs shared by every table hash
// in one hashSnapshotDatabase call, keeping hashTable/hashPaginatedTable under the
// linter's argument-count limit despite needing pool, batchSize, and retryProfile.
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

// writeLengthPrefixed writes an 8-byte big-endian length followed by the bytes.
// The length prefix prevents boundary collisions (e.g. "ab"+"cd" vs "abc"+"d").
func writeLengthPrefixed(h io.Writer, b []byte) {
	var lenBuf [8]byte
	binary.BigEndian.PutUint64(lenBuf[:], uint64(len(b)))
	_, _ = h.Write(lenBuf[:]) // sha256 Write never errors.
	_, _ = h.Write(b)
}
