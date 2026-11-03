/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/require"
	"github.com/yugabyte/pgx/v5"

	"github.com/hyperledger/fabric-x-committer/cmd/cliutil"
	"github.com/hyperledger/fabric-x-committer/cmd/config"
	"github.com/hyperledger/fabric-x-committer/utils/statedb"
	"github.com/hyperledger/fabric-x-committer/utils/testdb"
)

const (
	txIDFlagArg      = "--tx-id"
	cloneNameFlagArg = "--name"
)

func TestDatabaseCommands(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{databaseCommand, initDBCommand},
		{databaseCommand, cloneCommand, listClonesCommand},
		{databaseCommand, cloneCommand, deleteCloneCommand},
	} {
		t.Run(args[len(args)-1], func(t *testing.T) {
			t.Parallel()
			cmd, remaining, err := committerCMD().Find(args)
			require.NoError(t, err)
			require.Empty(t, remaining)
			require.Equal(t, args[len(args)-1], cmd.Name())
		})
	}
	for _, old := range []string{"init-db", "delete-clone"} {
		_, _, err := committerCMD().Find([]string{old})
		require.Error(t, err)
	}
}

func TestListClonesHelp(t *testing.T) {
	t.Parallel()
	cmd := listClonesCMD()
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"--help"})
	require.NoError(t, cmd.ExecuteContext(t.Context()))
	require.Contains(t, output.String(),
		"Corrupt records are omitted and reported as errors; the command exits nonzero.")
}

func TestWriteEmptySnapshotCloneList(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	require.NoError(t, writeSnapshotClones(&output, []*committerpb.SnapshotState{
		{TxRef: &committerpb.TxRef{TxId: "snap-cleared"}, Status: committerpb.SnapshotState_ABORTED},
	}))
	require.Equal(t, []string{"TX_ID", "CLONE_NAME", "STATUS"}, strings.Fields(output.String()))
	require.Equal(t, 1, strings.Count(output.String(), "\n"))
}

func TestDeleteCloneSelectors(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		flags     []string
		wantError string
	}{
		{name: "missing", wantError: "at least one of the flags in the group [tx-id name] is required"},
		{
			name: "both", flags: []string{txIDFlagArg, "tx", cloneNameFlagArg, "some-clone"},
			wantError: "none of the others can be",
		},
		{name: "empty transaction ID", flags: []string{txIDFlagArg, ""}, wantError: "exactly one nonempty"},
		{name: "empty name", flags: []string{cloneNameFlagArg, ""}, wantError: "exactly one nonempty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cmd := committerCMD()
			var output bytes.Buffer
			cmd.SetOut(&output)
			cmd.SetErr(&output)
			cmd.SetArgs(append([]string{databaseCommand, cloneCommand, deleteCloneCommand}, tc.flags...))
			require.ErrorContains(t, cmd.ExecuteContext(t.Context()), tc.wantError)
		})
	}
}

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

	pool, err := statedb.NewPool(t.Context(), dbConfig)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	insertRecord := func(state *committerpb.SnapshotState) {
		value, encodeErr := statedb.EncodeSnapshotState(state)
		require.NoError(t, encodeErr)
		_, insertErr := pool.Exec(t.Context(), fmt.Sprintf("INSERT INTO %s (key, value) VALUES ($1, $2)",
			statedb.TableName(committerpb.SnapshotNamespaceID)), []byte(state.TxRef.TxId), value)
		require.NoError(t, insertErr)
	}
	cloneExists := func(name string) bool {
		var exists bool
		require.NoError(t, pool.QueryRow(t.Context(),
			"SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)", name).Scan(&exists))
		return exists
	}
	insertRecord(&committerpb.SnapshotState{
		TxRef: &committerpb.TxRef{BlockNum: 1, TxId: txID}, Status: committerpb.SnapshotState_ABORTED,
		CloneDatabase: clone,
	})

	s := config.SystemConfig{DB: config.DatabaseConfig{
		Name: conn.Database, Username: conn.User, Password: conn.Password, Endpoints: conn.Endpoints,
	}}
	// Add a second clone to exercise name-based CLI deletion independently.
	nameClone := fmt.Sprintf("snapshot_cli_%d", rand.Uint32())
	t.Cleanup(func() {
		_ = statedb.AdminExec(context.Background(), dbConfig,
			"DROP DATABASE IF EXISTS "+pgx.Identifier{nameClone}.Sanitize())
	})
	require.NoError(t, statedb.AdminExec(t.Context(), dbConfig,
		"CREATE DATABASE "+pgx.Identifier{nameClone}.Sanitize()))
	for _, state := range []*committerpb.SnapshotState{
		{
			TxRef: &committerpb.TxRef{TxId: "snap-by-name"}, Status: committerpb.SnapshotState_CHECKPOINTED,
			CloneDatabase: nameClone, Hash: []byte("digest"),
		},
		{TxRef: &committerpb.TxRef{TxId: "snap-cleared"}, Status: committerpb.SnapshotState_ABORTED},
	} {
		insertRecord(state)
	}

	cmd := committerCMD()
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cfgPath := config.CreateTempConfigFromTemplate(t, config.TemplateVC, &s)
	cmd.SetArgs([]string{databaseCommand, cloneCommand, listClonesCommand, "--config", cfgPath})
	require.NoError(t, cmd.ExecuteContext(t.Context()))
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	require.Len(t, lines, 3)
	require.Equal(t, []string{"TX_ID", "CLONE_NAME", "STATUS"}, strings.Fields(lines[0]))
	require.Equal(t, []string{"snap-by-name", nameClone, "CHECKPOINTED"}, strings.Fields(lines[1]))
	require.Equal(t, []string{txID, clone, "ABORTED"}, strings.Fields(lines[2]))

	_, err = pool.Exec(t.Context(), fmt.Sprintf("INSERT INTO %s (key, value) VALUES ($1, $2)",
		statedb.TableName(committerpb.SnapshotNamespaceID)), []byte("corrupt"), []byte("not protobuf"))
	require.NoError(t, err)
	partialCMD := committerCMD()
	var partialOutput, diagnosticOutput bytes.Buffer
	partialCMD.SetOut(&partialOutput)
	partialCMD.SetErr(&diagnosticOutput)
	partialCMD.SetArgs([]string{databaseCommand, cloneCommand, listClonesCommand, "--config", cfgPath})
	require.ErrorContains(t, partialCMD.ExecuteContext(t.Context()), `corrupt _snapshot record for key "corrupt"`)
	require.Equal(t, output.String(), partialOutput.String(), "valid records must still be printed")
	require.Equal(t, 1, strings.Count(diagnosticOutput.String(), `corrupt _snapshot record for key "corrupt"`))
	_, err = pool.Exec(t.Context(), "DELETE FROM "+statedb.TableName(committerpb.SnapshotNamespaceID)+" WHERE key = $1",
		[]byte("corrupt"))
	require.NoError(t, err)

	for _, tc := range []cliutil.CommandTest{
		{
			Name:              "initializes the database through db init",
			Args:              []string{databaseCommand, initDBCommand},
			CmdStdOutput:      "Database initialized successfully",
			UseConfigTemplate: config.TemplateVC, System: s,
		},
		{
			Name:              "deletes by recorded name",
			Args:              []string{databaseCommand, cloneCommand, deleteCloneCommand, cloneNameFlagArg, nameClone},
			CmdStdOutput:      "Snapshot database clone " + nameClone + " deleted",
			UseConfigTemplate: config.TemplateVC, System: s,
		},
		{
			Name:              "deletes the clone",
			Args:              []string{databaseCommand, cloneCommand, deleteCloneCommand, txIDFlagArg, txID},
			CmdStdOutput:      "Snapshot database clone for tx snap-delete-cli deleted",
			UseConfigTemplate: config.TemplateVC,
			System:            s,
		},
		{
			Name:              "unknown tx-id",
			Args:              []string{databaseCommand, cloneCommand, deleteCloneCommand, txIDFlagArg, "no-such-tx"},
			UseConfigTemplate: config.TemplateVC,
			System:            s,
			CmdStdErrOutput:   "no snapshot record for the given transaction ID",
			Err: errors.New("failed to delete the snapshot database clone: failed to read _snapshot " +
				"record for tx no-such-tx: no-such-tx: no snapshot record for the given transaction ID"),
		},
	} {
		t.Run(tc.Name, func(t *testing.T) {
			cliutil.UnitTestRunner(t, committerCMD(), tc)
		})
	}
	require.False(t, cloneExists(clone), "the clone must be dropped")
	require.False(t, cloneExists(nameClone), "the named clone must be dropped")
	state, err := statedb.NewSnapshotStateManager(pool, dbConfig.Retry).Read(t.Context(), "snap-by-name")
	require.NoError(t, err)
	require.Empty(t, state.CloneDatabase)
	require.Equal(t, committerpb.SnapshotState_CHECKPOINTED, state.Status)
	require.Equal(t, []byte("digest"), state.Hash)
}
