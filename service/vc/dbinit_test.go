/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package vc

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/api/types"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

func TestDBInit(t *testing.T) {
	t.Parallel()
	env := NewDatabaseTestEnv(t)

	require.NoError(t, env.DB.setupSystemTablesAndNamespaces(t.Context()))
	tableName := nsTableNamePrefix + systemNamespaces[0]
	_, err := env.DB.pool.Exec(t.Context(), "insert into "+tableName+" values (UNNEST($1::bytea[]));", [][]byte{
		[]byte("tx1"), []byte("tx2"), []byte("tx3"), []byte("tx4"),
	})
	require.NoError(t, err)

	// Validate default values
	r, err := env.DB.pool.Query(t.Context(), "select * from "+tableName+";")
	require.NoError(t, err)
	defer r.Close()
	for r.Next() {
		var key, value, version []byte
		require.NoError(t, r.Scan(&key, &value, &version))
		require.Nil(t, value)
		require.Equal(t, types.VersionNumber(0).Bytes(), version)
	}
}

func TestRetry(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)
	pool, err := NewDatabasePool(
		ctx,
		&DatabaseConfig{
			Endpoints: []*connection.Endpoint{
				connection.CreateEndpoint(":1234"),
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
