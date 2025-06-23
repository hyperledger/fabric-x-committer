/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package vc

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.ibm.com/decentralized-trust-research/scalable-committer/api/types"
	"github.ibm.com/decentralized-trust-research/scalable-committer/utils/connection"
)

func TestDBInit(t *testing.T) {
	t.Parallel()
	env := newDatabaseTestEnvWithTablesSetup(t)

	tableName := nsTableNamePrefix + types.MetaNamespaceID
	keys := [][]byte{[]byte("tx1"), []byte("tx2"), []byte("tx3"), []byte("tx4")}
	ret := env.DB.pool.QueryRow(t.Context(), fmt.Sprintf(insertNsStatesSQLTempl, types.MetaNamespaceID), keys, keys)
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
