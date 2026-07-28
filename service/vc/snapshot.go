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
	"google.golang.org/protobuf/proto"

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
const maintenanceDBName = "postgres"

// rejectSnapshotIfPriorNotCheckpointed gates a new _snapshot request so that at
// most one snapshot lifecycle is active. The request is accepted only when the
// latest _snapshot record (tracked via latestSnapshotKeyMetadataKey) is
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
		if !nsWrites[committerpb.SnapshotNamespaceID].empty() {
			incomingTxID = txID
		}
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
		return nil // no prior snapshot, or the latest one is CHECKPOINTED -> accept.
	}

	vTx.updateInvalidTxs([]TxID{incomingTxID}, blockStatus)
	return nil
}

// determineSnapshotStatus looks up the latest _snapshot record via the
// latestSnapshotKeyMetadataKey pointer and returns the rejection status the
// incoming request should receive, or STATUS_UNSPECIFIED when the request may
// proceed (no prior snapshot ever accepted, or the latest one is CHECKPOINTED).
//
// A pointer that names a missing row, or a row whose value fails to decode, is
// an invariant violation (the pointer is written atomically with its row; see
// setLatestSnapshotKeyIfPresent) or storage corruption, not a normal rejection
// outcome. Both are returned as errors -- never silently mapped to a
// conservative rejection status -- so the batch fails/retries instead of
// masking the anomaly.
func (db *database) determineSnapshotStatus(ctx context.Context) (committerpb.Status, error) {
	state, err := db.readLatestSnapshotRecord(ctx)
	if err != nil {
		return committerpb.Status_STATUS_UNSPECIFIED, err
	}
	if state == nil {
		return committerpb.Status_STATUS_UNSPECIFIED, nil // no snapshot has ever been accepted.
	}

	switch state.Status {
	case committerpb.SnapshotState_CHECKPOINTED:
		return committerpb.Status_STATUS_UNSPECIFIED, nil
	case committerpb.SnapshotState_COMPLETED:
		return committerpb.Status_REJECTED_SNAPSHOT_NO_CHECKPOINT, nil
	default:
		return committerpb.Status_REJECTED_SNAPSHOT_IN_PROGRESS, nil
	}
}

// readLatestSnapshotRecord performs the full pointer-to-row read cycle: it looks
// up the latest-snapshot pointer (latestSnapshotKeyMetadataKey, set by
// setLatestSnapshotKeyIfPresent) and, when one is set, reads and decodes the
// _snapshot record it names.
//
// Returns (nil, nil) when no snapshot has ever been accepted (pointer unset).
// The pointer and its row are written atomically in the same DB transaction
// (setLatestSnapshotKeyIfPresent), so a pointer with no matching row is never
// possible under normal operation; this guards against a future bug (e.g. a
// maintenance path that deletes _snapshot rows without clearing the pointer)
// silently making the caller assume no active snapshot, by returning a hard
// error instead. A row whose value fails to decode is likewise a hard error,
// never silently mapped to a conservative rejection status.
func (db *database) readLatestSnapshotRecord(ctx context.Context) (*committerpb.SnapshotState, error) {
	key, err := retry.ExecuteWithResult(ctx, db.retryProfile, func() ([]byte, error) {
		r := db.pool.QueryRow(ctx, getMetadataPrepSQLStmt, latestSnapshotKeyMetadataKey)
		var v []byte
		return v, errors.Wrap(r.Scan(&v), "failed to get the latest snapshot key")
	})
	if err != nil {
		return nil, fmt.Errorf("failed to read latest snapshot key: %w", err)
	}
	if len(key) == 0 {
		return nil, nil
	}

	query := statedb.FmtNsID(queryValuesByKeysSQLTempl, committerpb.SnapshotNamespaceID)
	_, values, err := retryQueryAndReadTwoItems[[]byte, []byte](ctx, db, query, [][]byte{key})
	if err != nil {
		return nil, fmt.Errorf("failed to read latest _snapshot record: %w", err)
	}
	if len(values) == 0 {
		return nil, errors.Newf("latest snapshot key %s has no matching _snapshot record", key)
	}

	var state committerpb.SnapshotState
	if decodeErr := proto.Unmarshal(values[0], &state); decodeErr != nil {
		return nil, errors.Wrapf(decodeErr, "failed to decode latest _snapshot record for key %s", key)
	}
	return &state, nil
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
// this returns an error, batch is not committed, txID stays uncommitted, and
// coordinator retries snapshot (this PR does not self-recover).
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

// snapshotWriteInBatch returns the single _snapshot namespace write in newWrites, if
// any. A snapshot TX is submitted standalone, so at most one transaction in the batch
// carries a _snapshot write, and the preparer adds exactly one key/value pair
// (key = tx_id) for it. Shared by createSnapshotIfPresent and snapshotHashJobFromWrites
// so both extract the same record the same way.
func snapshotWriteInBatch(newWrites transactionToWrites) (*namespaceWrites, bool) {
	for _, nsWrites := range newWrites {
		w := nsWrites[committerpb.SnapshotNamespaceID]
		if !w.empty() {
			return w, true
		}
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
	state, err := decodeSnapshotState(recordValue)
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
	snapshotState, err := encodeSnapshotState(&committerpb.SnapshotState{
		TxRef:         ref,
		Status:        committerpb.SnapshotState_PENDING,
		CloneDatabase: snapshotDatabase,
	})
	if err != nil {
		return nil, errors.Wrapf(err, "failed to marshal PENDING snapshot state for database %s", snapshotDatabase)
	}
	return snapshotState, nil
}

// createSnapshotDatabase creates native zero-copy database named databaseName
// from source database. CREATE-only: it never DROPs. A "database already exists"
// result is treated as success (reuse) — name is deterministic and database
// content is drained deterministic cut, so a sibling VC's database is
// equivalent. Dropping here could delete a database whose txID has not yet
// committed, so it is forbidden.
//
// TODO: a hard-kill of the PostgreSQL path between
// ALLOW_CONNECTIONS false and the deferred re-enable locks out src for ALL
// pools. A VC cannot fix this (it may be dead; peers aren't authorized). The
// COORDINATOR, on detecting VC failure, re-enables ALLOW_CONNECTIONS via the
// maintenance DB. Not implemented in this PR.
func (db *database) createSnapshotDatabase(ctx context.Context, databaseName string) error {
	isYuga, err := statedb.IsYugabyteDB(ctx, db.pool)
	if err != nil {
		return err
	}
	src := pgx.Identifier{db.config.Database}.Sanitize()
	snapshotDatabase := pgx.Identifier{databaseName}.Sanitize()

	if isYuga {
		return db.createYugabyteSnapshotDatabase(ctx, snapshotDatabase, src)
	}
	return db.createPostgresSnapshotDatabase(ctx, snapshotDatabase, src)
}

// createYugabyteSnapshotDatabase uses DocDB cloning via CREATE DATABASE ... TEMPLATE; the
// source stays live. We clone as of current time (no AS OF): the sidecar drains
// before and after the snapshot TX and no user TX commits until the snapshot is
// fully processed, so "now" already is the exact snapshot cut. Cloning still
// requires a snapshot schedule to exist on the source keyspace (a standing PITR
// object provisioned out of band); without it YugabyteDB returns "Could not
// find snapshot schedule for namespace".
func (db *database) createYugabyteSnapshotDatabase(ctx context.Context, clone, src string) error {
	sql := fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s", clone, src)
	return ignoreDuplicateDatabase(db.adminExec(ctx, sql))
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

// ignoreDuplicateDatabase maps the "database already exists" error (PG SQLSTATE
// 42P04) to success: a concurrent sibling VC created the clone first, which is
// exactly the clone we need (reuse).
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

// snapshotDatabaseName returns deterministic database name for a snapshot at
// given TxRef. Name encodes block height so any VC (first attempt or
// coordinator-directed resubmission) targets same database.
func snapshotDatabaseName(ref *committerpb.TxRef) string {
	return fmt.Sprintf("snapshot_%d", ref.BlockNum)
}

// decodeSnapshotState unmarshals a _snapshot record value.
func decodeSnapshotState(raw []byte) (*committerpb.SnapshotState, error) {
	var state committerpb.SnapshotState
	if err := proto.Unmarshal(raw, &state); err != nil {
		return nil, errors.Wrap(err, "failed to decode _snapshot record")
	}
	return &state, nil
}

// encodeSnapshotState marshals a _snapshot record value.
func encodeSnapshotState(state *committerpb.SnapshotState) ([]byte, error) {
	raw, err := proto.Marshal(state)
	if err != nil {
		return nil, errors.Wrap(err, "failed to marshal _snapshot record")
	}
	return raw, nil
}
