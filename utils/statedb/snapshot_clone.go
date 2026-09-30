/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package statedb

import (
	"context"
	"fmt"
	"net"
	"slices"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/yugabyte/pgx/v5"
)

// maintenanceDBName is the neutral admin database that CREATE/DROP DATABASE runs against.
// A session cannot create or drop the database it is connected to on EITHER backend, so
// both PostgreSQL and YugabyteDB need a separate always-present database to issue them from.
//
// "postgres" is created by default on both backends (YugabyteDB ships it for PG
// compatibility) and is never a clone source. That matters because the PostgreSQL clone
// path blocks and terminates sessions on the SOURCE database only; running admin
// statements through "postgres" keeps this connection from being one of them.
const maintenanceDBName = "postgres"

var (
	// ErrSnapshotCloneInUse means the snapshot's clone may still be needed, so it is not deleted.
	ErrSnapshotCloneInUse = errors.New("snapshot clone is still in use")

	// deletableSnapshotStatuses are the statuses whose clone will never be hashed again.
	// Until then, the clone is the only artifact from which the hash can be recomputed.
	deletableSnapshotStatuses = []committerpb.SnapshotState_Status{
		committerpb.SnapshotState_CHECKPOINTED,
		committerpb.SnapshotState_ABORTED,
	}
)

// DeleteSnapshotClone drops the clone database of the snapshot txID and clears the
// record's clone_database, but keeps the record. It is safe to run while the committer
// is live, and on any node connected to the same database cluster.
//
// A record whose clone_database is already empty succeeds, so a repeated delete is a
// no-op. Otherwise the record must be CHECKPOINTED or ABORTED (ErrSnapshotCloneInUse).
//
// These steps cannot be atomic: PostgreSQL does not allow DROP DATABASE inside a
// transaction. Dropping first makes this safe to retry. If the state update fails, the
// record still names the deleted database; running it again skips the missing database
// (IF EXISTS) and finishes the update. Clearing the name first could leave a database
// that no record points to.
func DeleteSnapshotClone(ctx context.Context, config *Config, txID string) error {
	if txID == "" {
		return errors.New("tx_id must not be empty")
	}
	pool, err := NewPool(ctx, config)
	if err != nil {
		return errors.Wrap(err, "failed to connect to database")
	}
	defer pool.Close()
	snapshotState := NewSnapshotStateManager(pool, config.Retry)

	state, err := snapshotState.Read(ctx, txID)
	if err != nil {
		return fmt.Errorf("failed to read _snapshot record for tx %s: %w", txID, err)
	}
	if state.CloneDatabase == "" {
		return nil
	}
	// Check the status after the empty clone name so repeated deletes still succeed.
	if !slices.Contains(deletableSnapshotStatuses, state.Status) {
		return errors.Wrapf(ErrSnapshotCloneInUse, "tx %s is %s", txID, state.Status)
	}

	dropSQL := fmt.Sprintf("DROP DATABASE IF EXISTS %s", pgx.Identifier{state.CloneDatabase}.Sanitize())
	if err := AdminExec(ctx, config, dropSQL); err != nil {
		return fmt.Errorf("failed to drop snapshot database %s: %w", state.CloneDatabase, err)
	}

	// Clear the name only if the locked record still has the status checked above.
	update := SnapshotUpdate{
		Status:             state.Status,
		ExpectedStatus:     []committerpb.SnapshotState_Status{state.Status},
		ClearCloneDatabase: true,
	}
	if err := snapshotState.Update(ctx, state.TxRef, update); err != nil {
		return fmt.Errorf("failed to clear clone_database for snapshot tx %s: %w", txID, err)
	}
	return nil
}

// AdminExec runs one statement on a short-lived connection to the maintenance database.
// It does not retry: CREATE/DROP DATABASE are not idempotent at the SQL layer, and
// callers re-drive the whole operation instead.
//
// It uses a plain pgx.ConnConfig with a bounded dial timeout, not a pool, so an
// unreachable endpoint in a multi-host DSN fails fast instead of hanging for the
// context's full lifetime; pgx.Connect's default dialer has no timeout.
func AdminExec(ctx context.Context, config *Config, sql string) error {
	dsn, err := DataSourceName(DataSourceNameParams{
		Username:        config.Username,
		Password:        config.Password,
		Database:        maintenanceDBName,
		EndpointsString: config.EndpointsString(),
		LoadBalance:     config.LoadBalance,
		TLS:             config.TLS,
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
