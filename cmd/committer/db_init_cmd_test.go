/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/cmd/cliutil"
	"github.com/hyperledger/fabric-x-committer/cmd/config"
	"github.com/hyperledger/fabric-x-committer/utils/db"
	"github.com/hyperledger/fabric-x-committer/utils/testdb"
)

func TestMain(m *testing.M) {
	testdb.RunTestMain(m)
}

func TestInitStateDBCMD(t *testing.T) {
	t.Parallel()

	conn := testdb.PrepareTestEnv(t)

	cliutil.UnitTestRunner(t, committerCMD(), cliutil.CommandTest{
		Name:              initDBCommand,
		Args:              []string{initDBCommand},
		CmdStdOutput:      "Database initialized successfully",
		UseConfigTemplate: config.TemplateDBInit,
		System: config.SystemConfig{
			DB: config.DatabaseConfig{
				Endpoints:   conn.Endpoints,
				Name:        conn.Database,
				Username:    conn.User,
				Password:    conn.Password,
				LoadBalance: false,
			},
		},
	})

	t.Log("Command finished and succeeded, now verifying database initialization")

	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	t.Cleanup(cancel)
	pool, err := db.NewPool(ctx, &db.Config{
		Endpoints:      conn.Endpoints,
		Database:       conn.Database,
		Username:       conn.User,
		Password:       conn.Password,
		LoadBalance:    false,
		MaxConnections: 10,
		MinConnections: 1,
		Retry:          testdb.DefaultRetry,
	})
	require.NoError(t, err)
	defer pool.Close()

	t.Log("Querying for the tables created by initialization")
	// Check that system tables exist
	var count int
	require.NoError(t, pool.QueryRow(
		ctx, "SELECT COUNT(*) FROM pg_tables WHERE tablename IN ('tx_status', 'metadata')").Scan(&count),
	)
	require.Equal(t, 2, count, "system tables should exist")
}
