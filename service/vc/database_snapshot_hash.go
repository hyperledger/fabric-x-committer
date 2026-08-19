/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package vc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/yugabyte/pgx/v5"
	"github.com/yugabyte/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"github.com/hyperledger/fabric-x-committer/utils/retry"
	"github.com/hyperledger/fabric-x-committer/utils/statedb"
)

// snapshotStateUpdate bundles the fields updateSnapshotState can change. We
// use a struct instead of separate parameters because the linter caps
// functions at 4 arguments (ctx, ref, update already uses 3), and we need to
// also carry a diagnostic error message alongside status and digest.
// A zero-value field means "leave this part of the record unchanged": an
// empty ErrMsg leaves SnapshotState.Error as it was, and a nil Digest leaves
// SnapshotState.Hash as it was.
type snapshotStateUpdate struct {
	Status committerpb.SnapshotState_Status
	Digest []byte
	ErrMsg string
}

const (
	// snapshotCleanupTimeout bounds cleanup work that must run even after the
	// caller's context ended, so an unreachable database cannot block shutdown.
	snapshotCleanupTimeout = 5 * time.Second

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

// runSnapshotHashScheduler is the ONLY path that starts snapshot hashing. It
// periodically reads the latest durable _snapshot record and, when that record
// still needs hashing, claims it and hashes it inline. The commit path never
// starts a hash: it only has to make the record durable, and this loop picks it
// up on a later tick. One start path means a fresh snapshot, a resubmitted one,
// and one orphaned by a dead worker are all handled by the same code.
//
// Hashing runs inline rather than through a queue, so this loop is busy for the
// whole hash and cannot start a second one; the next tick is only considered once
// the current job returns.
//
// Every VC runs this same loop; there is no elected leader. When the lease
// expires, all VCs may notice on the same tick and try to start the job, but
// acquireSnapshotHashLease serializes them with SELECT ... FOR UPDATE: the first
// VC writes a fresh tokenized lease and takes the job, and every later VC reads
// that live lease and backs off. Database therefore picks one winner without
// coordinator involvement.
//
// The interval equals the configured lease TTL, and the first check happens one
// interval after start: a worker renews every TTL/4, so an active job always has
// a deadline comfortably beyond the next tick. If the worker dies, its last lease
// eventually expires and one of the VCs takes the job on a later tick. Hashing
// therefore starts within one TTL of a snapshot committing, which is the cost of
// having a single, coordinator-independent start path.
func (db *database) runSnapshotHashScheduler(ctx context.Context) error {
	ticker := time.NewTicker(db.snapshotHashLeaseTTL())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			// Errors are logged and retried on the next tick instead of stopping the
			// VC: scheduling is background work, normal transaction processing can
			// continue, and the durable row+lease remain for the next attempt.
			if err := db.hashLatestSnapshotIfNeeded(ctx); err != nil {
				logger.Errorf("failed to hash the latest snapshot: %+v", err)
			}
		}
	}
}

// hashLatestSnapshotIfNeeded hashes the latest _snapshot record when that record
// still needs it, using the status and clone database stored in the record rather
// than anything a caller supplies. There is at most one non-terminal snapshot at a
// time and the latest-snapshot pointer is written atomically with its row, so the
// latest record is always the one that needs hashing.
//
// A CHECKPOINTED or COMPLETED record has nothing to do, so it is left untouched.
// A record that is committed but has no clone_database is a broken state nothing
// can repair: a committed txID must always have a matching clone, and we must not
// clone again for an already-committed txID, so the record is marked FAILED with a
// diagnostic message and a halt is signalled -- once, on the transition, since a
// FAILED record is still re-read on every tick.
//
// The lease is what makes concurrent calls from several VCs safe. Failing to claim
// it is a success, not an error: some live worker already owns the job, or the
// snapshot turned out to be terminal.
func (db *database) hashLatestSnapshotIfNeeded(ctx context.Context) error {
	state, err := db.readLatestSnapshotRecord(ctx)
	if err != nil {
		return fmt.Errorf("failed to read latest _snapshot record: %w", err)
	}
	if state == nil {
		return nil // no snapshot has ever been accepted.
	}
	if state.TxRef == nil {
		return errors.New("corrupt latest _snapshot record: missing TxRef")
	}
	txID := state.TxRef.TxId

	switch state.Status {
	case committerpb.SnapshotState_CHECKPOINTED, committerpb.SnapshotState_COMPLETED:
		return nil // terminal / already done -- nothing to hash.
	case committerpb.SnapshotState_PENDING, committerpb.SnapshotState_IN_PROGRESS, committerpb.SnapshotState_FAILED:
		// fall through to the clone check below.
	default:
		return errors.Newf("_snapshot record for tx %s has unexpected status %s", txID, state.Status)
	}

	if state.CloneDatabase == "" {
		reason := fmt.Sprintf("tx %s is committed but has no clone_database to hash", txID)
		// FAILED is not terminal for the scheduler, so this record is re-read on every
		// tick. Write and warn only on the transition, or every VC would rewrite the row
		// and log the same halt once per TTL forever, which is the silent retry loop the
		// halt exists to avoid.
		if state.Status == committerpb.SnapshotState_FAILED && state.Error == reason {
			return nil
		}
		db.signalSnapshotHashHalt(reason)
		return db.updateSnapshotState(ctx, state.TxRef, snapshotStateUpdate{
			Status: committerpb.SnapshotState_FAILED,
			ErrMsg: reason,
		})
	}

	// Advisory fast path: while another worker plainly holds a live lease, skip the
	// acquire transaction rather than opening one per VC per tick just to block on
	// the lease row and roll back. acquireSnapshotHashLease remains the authority.
	heldByOther, err := db.snapshotHashLeaseHeldByOther(ctx)
	if err != nil {
		return err
	}
	if heldByOther {
		logger.Debugf("snapshot hash job for tx %s is already claimed; not hashing here", txID)
		return nil
	}

	claim, err := db.acquireSnapshotHashLease(ctx, txID)
	if err != nil {
		return err
	}
	if claim == nil {
		logger.Debugf("snapshot hash job for tx %s was claimed concurrently; not hashing here", txID)
		return nil
	}

	return db.hashSnapshotUnderClaim(ctx, claim, state.TxRef, state.CloneDatabase)
}

// signalSnapshotHashHalt records a condition nothing can repair automatically
// while starting a snapshot hash job.
//
// TODO: notify the coordinator instead of only logging, once a VC-to-coordinator
// halt notification exists. For now this logs a warning, similar to how the
// checkpoint-mismatch halt does not hide the problem behind a status change; the
// FAILED record written by hashLatestSnapshotIfNeeded is the visible signal in the
// meantime. The receiver is unused today, and stays on *database so that
// notification can use the connection without changing every call site.
func (*database) signalSnapshotHashHalt(reason string) {
	logger.Warnf("snapshot hash halt condition: %s", reason)
}

// hashSnapshotUnderClaim marks ref's snapshot IN_PROGRESS, hashes cloneDatabase,
// and publishes the digest, holding claim's lease throughout.
func (db *database) hashSnapshotUnderClaim(
	ctx context.Context, claim *snapshotHashLease, ref *committerpb.TxRef, cloneDatabase string,
) error {
	txID := claim.TxID
	// Clear the lease when we finish, so the next snapshot does not have to wait
	// out the remaining TTL. Skipping this on a crash is safe: the lease then
	// expires on its own, which is exactly how we detect a dead worker. The release
	// runs on a context detached from ctx so a lease is still freed when hashing was
	// cancelled, but bounded so an unreachable database cannot block shutdown.
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), snapshotCleanupTimeout)
		defer cancel()
		if _, err := db.updateSnapshotHashLease(cleanupCtx, claim, 0); err != nil {
			logger.Warnf("failed to release snapshot hash lease for tx %s: %+v", claim.TxID, err)
		}
	}()

	started, startErr := db.markSnapshotHashInProgress(ctx, claim, ref)
	if startErr != nil {
		return fmt.Errorf("failed to mark snapshot %s IN_PROGRESS: %w", cloneDatabase, startErr)
	}
	if !started {
		// The lease lapsed between acquiring it and this first write, and a successor
		// already owns the job. Stopping here is the point of the check: without it we
		// would drag a record the successor may have finished back to IN_PROGRESS.
		logger.Infof("snapshot hash job for tx %s was taken over before it started; not hashing here", txID)
		return nil
	}

	// Keep renewing the lease for as long as the hash runs, so a live worker is
	// never mistaken for a dead one and taken over. The two run as one group: a
	// takeover or a failed renewal cancels hashing, and hashing returning cancels
	// the renewal.
	g, gCtx := errgroup.WithContext(ctx)
	hashCtx, cancel := context.WithCancel(gCtx)
	defer cancel()

	var digest []byte
	g.Go(func() error {
		defer cancel()
		var err error
		digest, err = db.hasher.hashSnapshotDatabase(hashCtx, cloneDatabase)
		return errors.Wrapf(err, "failed to hash snapshot %s", cloneDatabase)
	})
	g.Go(func() error {
		return db.renewSnapshotHashLeaseUntilDone(hashCtx, claim)
	})
	if err := g.Wait(); err != nil {
		return fmt.Errorf("failed to hash snapshot %s under a held lease: %w", cloneDatabase, err)
	}

	completed, err := db.markSnapshotHashCompleted(ctx, claim, digest)
	if err != nil {
		return fmt.Errorf("failed to mark snapshot %s COMPLETED: %w", cloneDatabase, err)
	}
	if !completed {
		return errors.Newf("lost snapshot hash lease for tx %s before completion", txID)
	}
	return nil
}

// updateSnapshotState rewrites the _snapshot record for ref.TxId per update;
// TxRef and CloneDatabase are preserved because the existing record is decoded,
// mutated, and re-encoded rather than rebuilt.
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
	update snapshotStateUpdate,
) error {
	err := retry.Execute(ctx, db.retryProfile, func() error {
		tx, rollBackFunc, err := db.beginTx(ctx)
		if err != nil {
			return err
		}
		defer rollBackFunc()

		state, err := readAndLockSnapshotState(ctx, tx, ref.TxId)
		if err != nil {
			return err
		}

		state.Status = update.Status
		if update.Digest != nil {
			state.Hash = update.Digest
		}
		if update.ErrMsg != "" {
			state.Error = update.ErrMsg
		}

		if err := writeSnapshotState(ctx, tx, ref.TxId, state); err != nil {
			return err
		}
		return errors.Wrapf(tx.Commit(ctx), "failed to commit _snapshot state update for tx %s", ref.TxId)
	})
	return err //nolint:wrapcheck // already wrapped inside the retried closure.
}

// markSnapshotHashInProgress moves ref's snapshot to IN_PROGRESS only while claim
// still owns the lease, in one transaction under the lease and snapshot row locks.
//
// The ownership check is what keeps this write from corrupting a successor. A worker
// can lose its lease between acquiring it and reaching this first write -- it need
// only stall for one TTL -- by which time a successor may have hashed the snapshot
// and left it COMPLETED, or a checkpoint may have made it CHECKPOINTED. An unfenced
// status write would then drag that terminal record back to IN_PROGRESS: the digest
// stays correct, since markSnapshotHashCompleted is fenced too, but CHECKPOINTED is
// unrecoverable, because re-hashing only ever restores COMPLETED.
//
// It reports whether the row was advanced; false means the job now belongs to
// someone else and this worker must not hash.
func (db *database) markSnapshotHashInProgress(
	ctx context.Context,
	claim *snapshotHashLease,
	ref *committerpb.TxRef,
) (bool, error) {
	started, err := retry.ExecuteWithResult(ctx, db.retryProfile, func() (bool, error) {
		tx, rollBackFunc, err := db.beginTx(ctx)
		if err != nil {
			return false, err
		}
		defer rollBackFunc()

		// Lease before snapshot, the order every multi-row lease transaction uses.
		lease, now, err := readAndLockSnapshotHashLease(ctx, tx)
		if err != nil {
			return false, err
		}
		state, err := readAndLockSnapshotState(ctx, tx, ref.TxId)
		if err != nil {
			return false, err
		}
		if !lease.heldBy(claim, now) {
			return false, nil // taken over; leave the record to its owner.
		}
		// Our own re-entry after a lost commit ack: already IN_PROGRESS under our lease.
		if state.Status == committerpb.SnapshotState_IN_PROGRESS {
			return true, nil
		}

		state.Status = committerpb.SnapshotState_IN_PROGRESS
		if err := writeSnapshotState(ctx, tx, ref.TxId, state); err != nil {
			return false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return false, errors.Wrapf(err, "failed to commit snapshot IN_PROGRESS for tx %s", ref.TxId)
		}
		return true, nil
	})
	if err != nil {
		return false, fmt.Errorf("failed to start snapshot hash job for tx %s: %w", ref.TxId, err)
	}
	return started, nil
}

// markSnapshotHashCompleted publishes digest and clears claim's lease in one
// transaction, so no successor can claim the freed lease while the digest write is
// still uncommitted.
//
// It reports false when this worker no longer owns the job: the lease was taken
// over or cleared and the snapshot is not already COMPLETED with this digest, so
// the hash it computed must not be published. A cleared lease paired with our own
// published digest is instead reported as success, because that is a retry of a
// completion whose commit succeeded but whose result was lost. Any other state
// belongs to a successor worker and must not be overwritten.
//
//nolint:gocognit // one transaction: ownership check, digest write, lease clear.
func (db *database) markSnapshotHashCompleted(
	ctx context.Context,
	claim *snapshotHashLease,
	digest []byte,
) (bool, error) {
	completed, err := retry.ExecuteWithResult(ctx, db.retryProfile, func() (bool, error) {
		tx, rollBackFunc, err := db.beginTx(ctx)
		if err != nil {
			return false, err
		}
		defer rollBackFunc()

		lease, now, err := readAndLockSnapshotHashLease(ctx, tx)
		if err != nil {
			return false, err
		}
		state, err := readAndLockSnapshotState(ctx, tx, claim.TxID)
		if err != nil {
			return false, err
		}
		if !lease.heldBy(claim, now) {
			// A cleared lease paired with our own published digest is a retry of a
			// completion that committed but lost its result, so report success.
			return lease == nil &&
				state.Status == committerpb.SnapshotState_COMPLETED &&
				bytes.Equal(state.Hash, digest), nil
		}

		state.Status = committerpb.SnapshotState_COMPLETED
		state.Hash = digest
		if err := writeSnapshotState(ctx, tx, claim.TxID, state); err != nil {
			return false, err
		}
		if err := writeSnapshotHashLease(ctx, tx, nil); err != nil {
			return false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return false, errors.Wrapf(err, "failed to commit snapshot hash completion for tx %s", claim.TxID)
		}
		return true, nil
	})
	if err != nil {
		return false, fmt.Errorf("failed to complete snapshot hash job for tx %s: %w", claim.TxID, err)
	}
	return completed, nil
}

func readAndLockSnapshotState(
	ctx context.Context,
	tx pgx.Tx,
	txID string,
) (*committerpb.SnapshotState, error) {
	var raw []byte
	if err := tx.QueryRow(ctx, selectSnapshotRowForUpdateSQL, []byte(txID)).Scan(&raw); err != nil {
		return nil, errors.Wrapf(err, "failed to read _snapshot record for tx %s", txID)
	}
	state, err := decodeSnapshotState(raw)
	return state, errors.Wrapf(err, "tx %s", txID)
}

func writeSnapshotState(
	ctx context.Context,
	tx pgx.Tx,
	txID string,
	state *committerpb.SnapshotState,
) error {
	newRaw, err := encodeSnapshotState(state)
	if err != nil {
		return errors.Wrapf(err, "tx %s", txID)
	}

	_, err = tx.Exec(ctx, updateSnapshotRowSQL, []byte(txID), newRaw)
	return errors.Wrapf(err, "failed to update _snapshot record for tx %s", txID)
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
