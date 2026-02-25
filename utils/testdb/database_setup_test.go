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

func TestDefaultStartAndQuery(t *testing.T) {
	t.Parallel()
	if getDBDeploymentFromEnv() != deploymentLocal {
		t.Skip("the requested database is not local, " +
			"so, it will be skipped and tested in TestContainerStartAndQuery.")
	}
	StartAndQuery(t)
}

func TestContainerStartAndQuery(t *testing.T) {
	t.Skip("This test can run manually when needed")
	for _, tc := range []string{"postgres", "yugabyte"} {
		testCase := tc
		t.Run(testCase, func(t *testing.T) {
			t.Setenv(deploymentTypeEnv, "container")
			t.Setenv(databaseTypeEnv, testCase)
			StartAndQuery(t)
		})
	}
}

func StartAndQuery(t *testing.T) {
	t.Helper()
	connSettings := PrepareTestEnv(t)
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
