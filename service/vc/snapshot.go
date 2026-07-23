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

	"github.com/hyperledger/fabric-x-committer/utils/statedb"
)

// maintenanceDBName is the neutral admin database the clone runs against.
// A session cannot CREATE/DROP the database it is connected to on EITHER
// backend, so both PostgreSQL and YugabyteDB need a separate always-present DB
// to issue CREATE DATABASE from; that is what this connects to.
//
// "postgres" is chosen because it is created by default on both backends
// (YugabyteDB ships a "postgres" database for PG compatibility) and is never
// the clone source, so it is never locked out by the PostgreSQL
// ALLOW_CONNECTIONS dance. The extra PostgreSQL requirement that the TEMPLATE
// source be free of other sessions is handled in createPostgresSnapshotDatabase, not here,
// and does not apply to YugabyteDB (DocDB cloning keeps the source live).
const maintenanceDBName = "postgres"

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
	for _, nsWrites := range newWrites {
		w := nsWrites[committerpb.SnapshotNamespaceID]
		if w.empty() {
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
	}
	return nil
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
	var state committerpb.SnapshotState
	if err := proto.Unmarshal(recordValue, &state); err != nil {
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
	snapshotState, err := proto.Marshal(&committerpb.SnapshotState{
		TxRef:         ref,
		Status:        committerpb.SnapshotState_PENDING,
		CloneDatabase: snapshotDatabase,
	})
	if err != nil {
		return nil, errors.Wrapf(err, "failed to marshal PENDING snapshot state for database %s", snapshotDatabase)
	}
	return snapshotState, nil
}

// snapshotDatabaseName returns deterministic database name for a snapshot at
// given TxRef. Name encodes block height so any VC (first attempt or
// coordinator-directed resubmission) targets same database.
func snapshotDatabaseName(ref *committerpb.TxRef) string {
	return fmt.Sprintf("snapshot_%d", ref.BlockNum)
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
// to have no other sessions during the clone, so we block new connections and
// terminate existing ones, then re-allow (via defer, so it runs even on error).
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

	if _, err := conn.Exec(ctx, sql); err != nil {
		return errors.Wrapf(err, "failed to execute admin statement [%s]", sql)
	}
	return nil
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
