/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package vc

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/jackc/pgerrcode"
	"github.com/yugabyte/pgx/v5"
	"github.com/yugabyte/pgx/v5/pgconn"

	"github.com/hyperledger/fabric-x-committer/utils/retry"
	"github.com/hyperledger/fabric-x-committer/utils/statedb"
)

// maintenanceDBName is the neutral admin database the clone runs against.
// A session cannot CREATE/DROP the database it is connected to on EITHER
// backend, so both PostgreSQL and YugabyteDB need a separate always-present DB
// to issue CREATE DATABASE from; that is what this connects to.
//
// "postgres" is chosen because it is created by default on both backends
// (YugabyteDB ships a "postgres" database for PG compatibility) and is never
// the clone source. That matters because createPostgresSnapshotDatabase blocks
// and terminates sessions on the SOURCE database only (ALLOW_CONNECTIONS false /
// terminate-backends / ALLOW_CONNECTIONS true, see below) to satisfy PostgreSQL's
// TEMPLATE requirement that the source be free of other sessions; connecting
// admin operations through "postgres" instead of the source keeps this
// connection itself from being one of the sessions that gets terminated or
// locked out by that sequence. Does not apply to YugabyteDB (DocDB cloning
// keeps the source live, so no lockout dance is needed there).
const (
	maintenanceDBName = "postgres"

	yugabyteCloneStateComplete = "COMPLETE"
	yugabyteCloneStateAborted  = "ABORTED"
	yugabyteCloneStateQuery    = `
SELECT state, COALESCE(failure_reason, '')
FROM yb_database_clones()
WHERE db_name = $1`
)

// rejectSnapshotIfPriorNotCheckpointed gates a new _snapshot request so that at
// most one snapshot lifecycle is active. The request is accepted only when the
// latest _snapshot record (tracked via statedb.LatestSnapshotPointerKey) is
// CHECKPOINTED, or none exists yet; otherwise the incoming snapshot transaction
// is rejected WITHOUT creating a snapshot database or writing a _snapshot record.
//
// The incoming _snapshot write is removed from vTx.newWrites and its status is
// set in vTx.invalidTxStatus, so the normal status path reports the rejection
// and createSnapshotIfPresent then sees an empty newWrites and no-ops.
//
// Rejection status:
//   - COMPLETED latest record (finished but never checkpointed) -> NO_CHECKPOINT.
//   - any other non-CHECKPOINTED state (UNSPECIFIED/PENDING/IN_PROGRESS/FAILED)
//     or a record that fails to decode -> IN_PROGRESS (conservative).
//
// The latest-snapshot pointer is written atomically with the _snapshot row it
// targets (see setLatestSnapshotKeyIfPresent in database.go), so this lookup is
// always consistent: it never points at a row from a batch that did not commit,
// and by the time a new snapshot TX reaches this gate its own txID cannot yet be
// the pointer target (that only happens once ITS OWN commit succeeds, which is
// after this gate runs).
func (db *database) rejectSnapshotIfPriorNotCheckpointed(
	ctx context.Context, vTx *validatedTransactions,
) error {
	// A snapshot TX is submitted standalone (one transaction, one new-write entry).
	if len(vTx.newWrites) != 1 {
		return nil
	}
	var incomingTxID TxID
	for txID, nsWrites := range vTx.newWrites {
		w := nsWrites[committerpb.SnapshotNamespaceID]
		// An abort arrives precisely when the latest snapshot is not closed, so gating it
		// would make a stuck snapshot impossible to abandon.
		if w.empty() || committerpb.IsSnapshotAbortKey(w.keys[0]) {
			continue
		}
		incomingTxID = txID
	}
	if incomingTxID == "" {
		return nil
	}

	// Resubmission escape hatch: if the incoming txID already exists in tx_status,
	// this is a duplicate/resubmission owned by the existing dedup path. Do not
	// gate it, so it keeps its real committed status.
	rows, err := db.readStatusWithHeight(ctx, [][]byte{[]byte(incomingTxID)})
	if err != nil {
		return fmt.Errorf("failed to read status for snapshot tx %s: %w", incomingTxID, err)
	}
	if len(rows) > 0 {
		return nil
	}

	blockStatus, err := db.determineSnapshotStatus(ctx)
	if err != nil {
		return err
	}
	if blockStatus == committerpb.Status_STATUS_UNSPECIFIED {
		return nil // no prior snapshot, or the latest one's lifecycle is closed -> accept.
	}

	vTx.updateInvalidTxs([]TxID{incomingTxID}, blockStatus)
	return nil
}

// determineSnapshotStatus looks up the latest _snapshot record via the
// statedb.LatestSnapshotPointerKey pointer and returns the rejection status the
// incoming request should receive, or STATUS_UNSPECIFIED when the request may
// proceed (no prior snapshot ever accepted, or the latest one's lifecycle is closed).
//
// Closed means CHECKPOINTED or ABORTED: nothing further will happen to that snapshot.
// ABORTED is the only exit for a snapshot that can never be hashed, which would otherwise
// reject every later request forever.
//
// A pointer that names a missing row, or a row whose value fails to decode, is
// an invariant violation (the pointer is written atomically with its row; see
// setLatestSnapshotKeyIfPresent) or storage corruption, not a normal rejection
// outcome. Both are returned as errors -- never silently mapped to a
// conservative rejection status -- so the batch fails/retries instead of
// masking the anomaly.
func (db *database) determineSnapshotStatus(ctx context.Context) (committerpb.Status, error) {
	state, err := db.snapshotState.ReadLatest(ctx)
	if err != nil {
		return committerpb.Status_STATUS_UNSPECIFIED, err
	}
	if state == nil {
		return committerpb.Status_STATUS_UNSPECIFIED, nil // no snapshot has ever been accepted.
	}

	switch state.Status {
	case committerpb.SnapshotState_CHECKPOINTED, committerpb.SnapshotState_ABORTED:
		return committerpb.Status_STATUS_UNSPECIFIED, nil
	case committerpb.SnapshotState_COMPLETED:
		return committerpb.Status_REJECTED_SNAPSHOT_NO_CHECKPOINT, nil
	default:
		return committerpb.Status_REJECTED_SNAPSHOT_IN_PROGRESS, nil
	}
}

// snapshotAbortTx is a batch's single abort write: the TX to reject against, and the
// snapshot it abandons.
type snapshotAbortTx struct {
	txID     TxID
	blockNum uint64
}

// rejectSnapshotAbortIfNoSuchSnapshot lets an abort commit only when it names a snapshot
// this committer can still abandon.
//
// An abort attests to nothing, so no failure here halts intake: every one is bad input,
// rejected per TX. Since anyone satisfying SnapshotEndorsement may submit one, halting
// instead would let an authorized submitter stop the committer.
//
// The form is already validated by the sidecar (checkSnapshotNamespace).
func (db *database) rejectSnapshotAbortIfNoSuchSnapshot(
	ctx context.Context, vTx *validatedTransactions,
) error {
	abort, err := snapshotAbortWriteInBatch(vTx.newWrites)
	if err != nil {
		return err
	}
	if abort == nil {
		return nil
	}

	rejection, err := db.determineSnapshotAbortRejection(ctx, abort)
	if err != nil {
		return err
	}
	if rejection != committerpb.Status_STATUS_UNSPECIFIED {
		// Drops the write, so no abort row becomes durable for an abort we did not accept.
		vTx.updateInvalidTxs([]TxID{abort.txID}, rejection)
		return nil
	}

	vTx.snapshotAbort = abort
	return nil
}

// determineSnapshotAbortRejection returns the status an abort must be rejected with, or
// STATUS_UNSPECIFIED when it names a snapshot we can still abandon.
//
// It compares against the latest record, not a lookup by block, because a new snapshot is
// admitted only once the previous one closes -- so a valid abort always names the latest.
//
// Every status short of closed is abortable, COMPLETED included: a diverged digest is
// exactly a snapshot nobody will checkpoint, and refusing it would leave no exit. The two
// closed statuses get their own rejection so an administrator can tell a snapshot that does
// not exist from one that is already abandoned.
func (db *database) determineSnapshotAbortRejection(
	ctx context.Context, abort *snapshotAbortTx,
) (committerpb.Status, error) {
	state, err := db.snapshotState.ReadLatest(ctx)
	if err != nil {
		return committerpb.Status_STATUS_UNSPECIFIED, err
	}

	switch {
	case state == nil || state.TxRef == nil:
		logger.Warnf("Rejecting abort TX [%s] for block [%d]: no _snapshot record exists to abort",
			abort.txID, abort.blockNum)
		return committerpb.Status_REJECTED_NO_SUCH_SNAPSHOT, nil
	case state.TxRef.BlockNum != abort.blockNum:
		logger.Warnf("Rejecting abort TX [%s]: the snapshot awaiting a checkpoint is at block [%d], not [%d]",
			abort.txID, state.TxRef.BlockNum, abort.blockNum)
		return committerpb.Status_REJECTED_NO_SUCH_SNAPSHOT, nil
	case state.Status == committerpb.SnapshotState_CHECKPOINTED:
		logger.Warnf("Rejecting abort TX [%s] for block [%d]: the snapshot was already checkpointed",
			abort.txID, abort.blockNum)
		return committerpb.Status_REJECTED_SNAPSHOT_ALREADY_CHECKPOINTED, nil
	case state.Status == committerpb.SnapshotState_ABORTED:
		logger.Warnf("Rejecting abort TX [%s] for block [%d]: the snapshot was already aborted",
			abort.txID, abort.blockNum)
		return committerpb.Status_REJECTED_SNAPSHOT_ALREADY_ABORTED, nil
	default:
		return committerpb.Status_STATUS_UNSPECIFIED, nil
	}
}

// snapshotAbortWriteInBatch returns the batch's abort write, if any.
//
// A second abort fails the whole batch rather than being skipped: a skipped write still
// commits, unverified, which is what this check exists to prevent, and there is no basis
// for picking which abort is real. Nothing produces two today, so it is a broken
// invariant, not bad input.
func snapshotAbortWriteInBatch(newWrites transactionToWrites) (*snapshotAbortTx, error) {
	var abort *snapshotAbortTx
	for txID, nsWrites := range newWrites {
		w := nsWrites[committerpb.SnapshotNamespaceID]
		if w.empty() || !committerpb.IsSnapshotAbortKey(w.keys[0]) {
			continue
		}
		blockNum, err := committerpb.BlockNumFromSnapshotAbortKey(w.keys[0])
		if err != nil {
			return nil, errors.Wrapf(err, "abort TX %s has an undecodable key", txID)
		}

		if abort != nil {
			return nil, errors.Newf(
				"a batch carries at most one snapshot abort, but it has both TX %s and TX %s", abort.txID, txID,
			)
		}
		abort = &snapshotAbortTx{txID: txID, blockNum: blockNum}
	}
	return abort, nil
}

// createSnapshotIfPresent detects a _snapshot record in the batch's
// per-transaction new-writes and, BEFORE the batch is committed, creates the
// snapshot database and rewrites the record's value to a PENDING SnapshotState
// carrying that database name. The rewritten value is then persisted atomically
// with the snapshot txID by the normal db.commit path, giving the invariant
// txID committed <=> snapshot database exists <=> PENDING record.
//
// The sidecar drains before and after a snapshot TX, so it is submitted
// standalone: its batch holds exactly one transaction (one new-write entry) and
// the preparer adds exactly one _snapshot record (key = tx_id) for it. We short-
// circuit unless newWrites has exactly one entry and act on that single record
// rather than scanning every write.
//
// The incoming record value carries only TxRef with status UNSPECIFIED (the
// preparer sets no status). This function is called exactly once per batch,
// before the committer's retry loop, so it neither reads a status back from the
// _snapshot table nor re-observes its own PENDING rewrite.
//
// Snapshot database creation MUST succeed before txID is committed. On failure
// this returns an error, batch is not committed, txID stays uncommitted, and the
// coordinator retries the snapshot; this path does not self-recover.
func (db *database) createSnapshotIfPresent(ctx context.Context, newWrites transactionToWrites) error {
	// A snapshot TX is submitted standalone: the sidecar drains before and after it,
	// so its batch contains exactly one transaction and hence one new-write entry.
	// Any other count means there is no _snapshot record to act on.
	if len(newWrites) != 1 {
		return nil
	}
	w, ok := snapshotWriteInBatch(newWrites)
	if !ok {
		return nil
	}

	// Exactly one key: the preparer adds a single _snapshot record per snapshot TX.
	snapshotState, err := db.createSnapshotDatabaseAndRewriteRecord(ctx, w.keys[0], w.values[0])
	if err != nil {
		return err
	}
	if snapshotState != nil {
		w.values[0] = snapshotState
	}
	return nil
}

// snapshotWriteInBatch returns the single snapshot-request write in newWrites, if any. A
// snapshot request is submitted standalone, so at most one transaction in the batch
// carries one, and the preparer adds exactly one key/value pair (key = tx_id) for it.
//
// An abort shares this namespace but is excluded: it abandons a snapshot, so cloning for
// it would be backwards and would leak a clone no record names.
func snapshotWriteInBatch(newWrites transactionToWrites) (*namespaceWrites, bool) {
	for _, nsWrites := range newWrites {
		w := nsWrites[committerpb.SnapshotNamespaceID]
		if w.empty() || committerpb.IsSnapshotAbortKey(w.keys[0]) {
			continue
		}
		return w, true
	}
	return nil, false
}

// createSnapshotDatabaseAndRewriteRecord decodes one _snapshot record, creates
// or reuses its snapshot database when needed, and returns a rewritten PENDING
// record. A nil result means leave recordValue unchanged.
//
// The tx_status lookup and CREATE-only database operation handle these cases:
//
//	snapshot case                                txID in tx_status  database action            record result
//	fresh snapshot                               no                 create                    rewritten PENDING
//	retry after failed database creation         no                 create                    rewritten PENDING
//	retry after database creation, before commit no                 CREATE; duplicate = reuse rewritten PENDING
//	resubmission or duplicate txID               yes                do not create or reuse     unchanged
//
// A txID already in tx_status may be a same-height resubmission or a
// different-height duplicate. The normal commit path resolves that distinction;
// this function must not create a snapshot database for either case.
func (db *database) createSnapshotDatabaseAndRewriteRecord(
	ctx context.Context, key, recordValue []byte,
) ([]byte, error) {
	state, err := statedb.DecodeSnapshotState(recordValue)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to decode _snapshot record for key %s", key)
	}

	ref := state.TxRef

	if ref == nil {
		return nil, errors.Newf("_snapshot record for key %s has no TxRef", key)
	}

	// Skip database creation unless this is first-ever submission (txID absent
	// from tx_status). If txID exists at SAME height it is a resubmission whose
	// snapshot database was created in a prior life; if it exists at DIFFERENT
	// height snapshot TX is rejected as duplicate and must not leave an orphan
	// database behind. Either way leave record unchanged; commit path returns
	// correct status.
	rows, err := db.readStatusWithHeight(ctx, [][]byte{[]byte(ref.TxId)})
	if err != nil {
		return nil, fmt.Errorf("failed to read status for snapshot tx %s: %w", ref.TxId, err)
	}
	if len(rows) > 0 {
		return nil, nil
	}

	snapshotDatabase := snapshotDatabaseName(ref)
	if createErr := db.createSnapshotDatabase(ctx, snapshotDatabase); createErr != nil {
		return nil, fmt.Errorf("failed to create snapshot database %s: %w", snapshotDatabase, createErr)
	}

	// PENDING record rewrite: written atomically with the snapshot txID by the
	// normal db.commit path once the snapshot database exists.
	snapshotState, err := statedb.EncodeSnapshotState(&committerpb.SnapshotState{
		TxRef:         ref,
		Status:        committerpb.SnapshotState_PENDING,
		CloneDatabase: snapshotDatabase,
	})
	if err != nil {
		return nil, errors.Wrapf(err, "failed to marshal PENDING snapshot state for database %s", snapshotDatabase)
	}
	return snapshotState, nil
}

// createSnapshotDatabase creates or reuses a native zero-copy database. A duplicate
// name is a reuse candidate: YugabyteDB still verifies clone completion before success.
// Name is deterministic and database content is a drained deterministic cut, so a
// sibling VC's complete database is equivalent. Dropping here could delete a database
// whose txID has not yet committed, so it is forbidden.
//
// TODO: a hard-kill of the PostgreSQL path between
// ALLOW_CONNECTIONS false and the deferred re-enable locks out src for ALL
// pools. A VC cannot fix this (it may be dead; peers aren't authorized). The
// COORDINATOR, on detecting VC failure, re-enables ALLOW_CONNECTIONS via the
// maintenance DB. Not implemented yet.
func (db *database) createSnapshotDatabase(ctx context.Context, databaseName string) error {
	isYuga, err := statedb.IsYugabyteDB(ctx, db.pool)
	if err != nil {
		return err
	}
	if isYuga {
		return db.createYugabyteSnapshotDatabase(ctx, databaseName, db.config.Database)
	}

	src := pgx.Identifier{db.config.Database}.Sanitize()
	snapshotDatabase := pgx.Identifier{databaseName}.Sanitize()
	return db.createPostgresSnapshotDatabase(ctx, snapshotDatabase, src)
}

// createYugabyteSnapshotDatabase uses DocDB cloning and waits for YugabyteDB's clone
// catalog to report COMPLETE after either creation or duplicate-name reuse. Source stays
// live. We clone as of current time (no AS OF): sidecar drains before and after snapshot
// TX and no user TX commits until snapshot is fully processed, so "now" is exact cut.
// Cloning requires a snapshot schedule on source keyspace; without it YugabyteDB returns
// "Could not find snapshot schedule for namespace".
func (db *database) createYugabyteSnapshotDatabase(ctx context.Context, cloneName, srcName string) error {
	clone := pgx.Identifier{cloneName}.Sanitize()
	src := pgx.Identifier{srcName}.Sanitize()
	sql := fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s", clone, src)
	if err := ignoreDuplicateDatabase(db.adminExec(ctx, sql)); err != nil {
		return err
	}
	return db.waitForYugabyteClone(ctx, cloneName)
}

func (db *database) waitForYugabyteClone(ctx context.Context, clone string) error {
	_, err := retry.ExecuteWithResult(ctx, db.retryProfile, func() (any, error) {
		var state, failureReason string
		err := db.pool.QueryRow(ctx, yugabyteCloneStateQuery, clone).Scan(&state, &failureReason)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to read state for YugabyteDB clone %s", clone)
		}
		return nil, yugabyteCloneStateError(clone, state, failureReason)
	}, retry.ErrNonRetryable)
	return err
}

// createPostgresSnapshotDatabase uses STRATEGY=FILE_COPY. PostgreSQL requires the source
// to have no other sessions during the clone, so this runs a three-step sequence:
// ALTER DATABASE ... ALLOW_CONNECTIONS false to block new sessions, terminate
// existing backends via pg_terminate_backend, then CREATE DATABASE ... TEMPLATE ...
// STRATEGY=FILE_COPY; ALLOW_CONNECTIONS is re-enabled via defer so it runs even on error.
func (db *database) createPostgresSnapshotDatabase(ctx context.Context, clone, src string) error {
	if err := db.adminExec(ctx, fmt.Sprintf("ALTER DATABASE %s ALLOW_CONNECTIONS false", src)); err != nil {
		return err
	}
	// Re-enable even if CREATE DATABASE fails, so the source is never left locked
	// out on the happy/soft-error path. (Hard-kill lockout is coordinator-recovered.)
	defer func() { //nolint:contextcheck // re-enable must run even if ctx is already cancelled/expired.
		if err := db.adminExec(context.Background(),
			fmt.Sprintf("ALTER DATABASE %s ALLOW_CONNECTIONS true", src)); err != nil {
			logger.Warnf("failed to re-enable connections on source database: %+v", err)
		}
	}()

	// terminate uses a string-built literal (not a parameterized query) because
	// adminExec takes a bare SQL string; db.config.Database is server-configured,
	// not attacker input, so quote-doubling escaping is sufficient here.
	terminate := fmt.Sprintf(
		"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '%s' AND pid <> pg_backend_pid()",
		strings.ReplaceAll(db.config.Database, "'", "''"),
	)
	if err := db.adminExec(ctx, terminate); err != nil {
		return err
	}

	sql := fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s STRATEGY=FILE_COPY", clone, src)
	return ignoreDuplicateDatabase(db.adminExec(ctx, sql))
}

// adminExec opens a short-lived dedicated connection to the maintenance DB
// (outside the pgxpool) and runs a single statement. Used for CREATE DATABASE
// and the PostgreSQL ALTER DATABASE dance, which cannot run on the source pool.
//
// Unlike the rest of this package, adminExec deliberately does NOT wrap the call
// in db.retryProfile. The admin statements are one-shot DDL whose errors are
// deterministic and semantically meaningful to the caller: "database already
// exists" (PG SQLSTATE 42P04) is mapped to success by ignoreDuplicateDatabase,
// and a missing template or bad name is a permanent failure. Retrying would
// either loop on a permanent error until the context deadline or defeat that
// mapping. The clone flow is instead re-driven end-to-end by the coordinator on
// VC failure.
//
// Uses a bounded dial timeout (via a plain pgx.ConnConfig, not pgxpool) so an
// unreachable endpoint in a multi-host DSN fails fast instead of hanging for
// the context's full lifetime — pgx.Connect's default dialer has no timeout.
func (db *database) adminExec(ctx context.Context, sql string) error {
	c := db.config
	dsn, err := statedb.DataSourceName(statedb.DataSourceNameParams{
		Username:        c.Username,
		Password:        c.Password,
		Database:        maintenanceDBName,
		EndpointsString: c.EndpointsString(),
		LoadBalance:     c.LoadBalance,
		TLS:             c.TLS,
	})
	if err != nil {
		return err
	}
	connConfig, err := pgx.ParseConfig(dsn)
	if err != nil {
		return errors.Wrap(err, "failed to parse maintenance-db DSN")
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	connConfig.DialFunc = dialer.DialContext

	conn, err := pgx.ConnectConfig(ctx, connConfig)
	if err != nil {
		return errors.Wrap(err, "failed to open maintenance-db admin connection")
	}
	defer func() { _ = conn.Close(ctx) }()

	_, err = conn.Exec(ctx, sql)
	return errors.Wrapf(err, "failed to execute admin statement [%s]", sql)
}

// ignoreDuplicateDatabase maps 42P04 to nil so backend-specific callers can reuse
// a deterministic clone name. YugabyteDB callers must still verify clone readiness.
func ignoreDuplicateDatabase(err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgerrcode.DuplicateDatabase {
		return nil
	}
	return err
}

func yugabyteCloneStateError(clone, state, failureReason string) error {
	switch state {
	case yugabyteCloneStateComplete:
		return nil
	case yugabyteCloneStateAborted:
		if failureReason == "" {
			failureReason = "not reported"
		}
		return errors.Wrapf(
			retry.ErrNonRetryable,
			"YugabyteDB clone %s aborted; failure reason: %s",
			clone,
			failureReason,
		)
	default:
		return errors.Newf("YugabyteDB clone %s is not ready: state %s", clone, state)
	}
}

// snapshotDatabaseName returns deterministic database name for a snapshot at
// given TxRef. Name encodes block height so any VC (first attempt or
// coordinator-directed resubmission) targets same database.
func snapshotDatabaseName(ref *committerpb.TxRef) string {
	return fmt.Sprintf("snapshot_%d", ref.BlockNum)
}
