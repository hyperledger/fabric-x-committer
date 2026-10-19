/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/require"
	"github.com/yugabyte/pgx/v5"

	"github.com/hyperledger/fabric-x-committer/cmd/cliutil"
	"github.com/hyperledger/fabric-x-committer/cmd/config"
	"github.com/hyperledger/fabric-x-committer/utils/statedb"
	"github.com/hyperledger/fabric-x-committer/utils/testdb"
)

//nolint:paralleltest // Cannot parallelize due to logger.
func TestDeleteCloneCMD(t *testing.T) {
	require.NoError(t, testdb.SetupSharedContainer())
	t.Cleanup(testdb.CleanupSharedContainer)

	conn := testdb.PrepareTestEnv(t)
	dbConfig := &statedb.Config{
		Endpoints: conn.Endpoints, Username: conn.User, Password: conn.Password,
		Database: conn.Database, MaxConnections: 1, TLS: conn.TLS, Retry: testdb.DefaultRetry,
	}
	require.NoError(t, statedb.SetupSystemTablesAndNamespaces(t.Context(), dbConfig))

	// An ABORTED record whose clone exists. Deletion rules are covered by the statedb
	// tests; this checks the command's wiring from config to the dropped database.
	const txID = "snap-delete-cli"
	clone := fmt.Sprintf("snapshot_1_%d", rand.Uint32())
	cloneSQL := pgx.Identifier{clone}.Sanitize()
	t.Cleanup(func() {
		_ = statedb.AdminExec(context.Background(), dbConfig, "DROP DATABASE IF EXISTS "+cloneSQL)
	})
	require.NoError(t, statedb.AdminExec(t.Context(), dbConfig, "CREATE DATABASE "+cloneSQL))

	raw, err := statedb.EncodeSnapshotState(&committerpb.SnapshotState{
		TxRef:         &committerpb.TxRef{BlockNum: 1, TxId: txID},
		Status:        committerpb.SnapshotState_ABORTED,
		CloneDatabase: clone,
	})
	require.NoError(t, err)
	pool, err := statedb.NewPool(t.Context(), dbConfig)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(t.Context(), fmt.Sprintf("INSERT INTO %s (key, value) VALUES ($1, $2)",
		statedb.TableName(committerpb.SnapshotNamespaceID)), []byte(txID), raw)
	require.NoError(t, err)
	cloneExists := func() bool {
		var exists bool
		require.NoError(t, pool.QueryRow(t.Context(),
			"SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)", clone).Scan(&exists))
		return exists
	}

	s := config.SystemConfig{DB: config.DatabaseConfig{
		Name: conn.Database, Username: conn.User, Password: conn.Password, Endpoints: conn.Endpoints,
	}}
	for _, tc := range []cliutil.CommandTest{
		{
			Name:              "deletes the clone",
			Args:              []string{deleteCloneCommand, "--tx-id", txID},
			CmdStdOutput:      "Snapshot database clone for tx snap-delete-cli deleted",
			UseConfigTemplate: config.TemplateVC,
			System:            s,
		},
		{
			Name:              "unknown tx-id",
			Args:              []string{deleteCloneCommand, "--tx-id", "no-such-tx"},
			UseConfigTemplate: config.TemplateVC,
			System:            s,
			CmdStdErrOutput:   "no snapshot record for the given transaction ID",
			Err: errors.New("failed to delete the snapshot database clone: failed to read _snapshot " +
				"record for tx no-such-tx: no-such-tx: no snapshot record for the given transaction ID"),
		},
		{
			Name: "tx-id is required",
			Args: []string{deleteCloneCommand},
			Err:  errors.New(`required flag(s) "tx-id" not set`),
		},
	} {
		t.Run(tc.Name, func(t *testing.T) {
			cliutil.UnitTestRunner(t, committerCMD(), tc)
		})
	}
	require.False(t, cloneExists(), "the clone must be dropped")
}
