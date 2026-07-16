/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package statedb

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/retry"
)

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
	_, err := NewPool(
		ctx,
		&Config{
			Endpoints:      []*connection.Endpoint{{Port: 1234}},
			Username:       "name",
			Password:       "pwd",
			MaxConnections: 5,
			Retry: &retry.Profile{
				MaxElapsedTime: 10 * time.Second,
			},
		},
	)
	require.ErrorContains(t, err, "failed to create a connection pool")
}
