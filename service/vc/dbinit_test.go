/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package vc

import (
	"context"
	"testing"
	"time"

	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

func TestDBInit(t *testing.T) {
	t.Parallel()
	env := newDatabaseTestEnvWithTablesSetup(t)

	tableName := TableName(committerpb.MetaNamespaceID)
	keys := [][]byte{[]byte("tx1"), []byte("tx2"), []byte("tx3"), []byte("tx4")}
	ret := env.DB.pool.QueryRow(t.Context(), FmtNsID(insertNsStatesSQLTempl, committerpb.MetaNamespaceID), keys, keys)
	duplicates, err := readArrayResult[[]byte](ret)
	require.NoError(t, err)
	require.Empty(t, duplicates)

	// Validate default values
	r, err := env.DB.pool.Query(t.Context(), "select * from "+tableName+";")
	require.NoError(t, err)
	defer r.Close()
	for r.Next() {
		var key, value []byte
		var version uint64
		require.NoError(t, r.Scan(&key, &value, &version))
		require.NotNil(t, key)
		require.Equal(t, key, value)
		require.EqualValues(t, 0, version)
	}
}

func TestFmtSplitIntoTablets(t *testing.T) {
	t.Parallel()

	t.Run("init_database_tmpl with tablets", func(t *testing.T) {
		t.Parallel()
		result := fmtSplitIntoTablets(dbInitSQLStmt, 4)
		require.Contains(t, result, "SPLIT INTO 4 TABLETS")
		require.NotContains(t, result, splitIntoTabletsPlaceholder)
	})

	t.Run("init_database_tmpl without tablets", func(t *testing.T) {
		t.Parallel()
		result := fmtSplitIntoTablets(dbInitSQLStmt, 0)
		require.NotContains(t, result, "SPLIT INTO")
		require.NotContains(t, result, splitIntoTabletsPlaceholder)
	})

	t.Run("create_namespace_tmpl with tablets", func(t *testing.T) {
		t.Parallel()
		result := fmtSplitIntoTablets(createNamespaceSQLStmt, 3)
		require.Contains(t, result, "SPLIT INTO 3 TABLETS")
		require.NotContains(t, result, splitIntoTabletsPlaceholder)
	})

	t.Run("create_namespace_tmpl without tablets", func(t *testing.T) {
		t.Parallel()
		result := fmtSplitIntoTablets(createNamespaceSQLStmt, 0)
		require.NotContains(t, result, "SPLIT INTO")
		require.NotContains(t, result, splitIntoTabletsPlaceholder)
	})
}

func TestRetry(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)
	pool, err := NewDatabasePool(
		ctx,
		&DatabaseConfig{
			Endpoints: []*connection.Endpoint{
				test.MustCreateEndpoint(":1234"),
			},
			Username:       "name",
			Password:       "pwd",
			MaxConnections: 5,
			Retry: &connection.RetryProfile{
				MaxElapsedTime: 10 * time.Second,
			},
		})
	require.ErrorContains(t, err, "failed to create a connection pool")
	require.Nil(t, pool)
}
