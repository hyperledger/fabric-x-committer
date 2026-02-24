/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package testdb

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ConnectAndQueryTest is an exported function for testing purpose.
func ConnectAndQueryTest(t *testing.T, connections *Connection) {
	t.Helper()
	connSettings := PrepareTestEnvWithConnection(t, connections)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	conn, err := connSettings.open(ctx)
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.Ping(ctx))

	rows, err := conn.Query(ctx, "select distinct tablename from pg_catalog.pg_tables;")
	require.NoError(t, err)
	defer rows.Close()

	var allTables []string
	for rows.Next() {
		require.NoError(t, rows.Err())
		var tableName string
		require.NoError(t, rows.Scan(&tableName))
		allTables = append(allTables, tableName)
	}
	t.Logf("tables: %s", allTables)
}
