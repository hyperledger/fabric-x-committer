/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package vc

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

func TestDatasourceName(t *testing.T) {
	t.Parallel()
	c1 := &DatabaseConfig{
		Endpoints: []*connection.Endpoint{
			test.MustCreateEndpoint("localhost:5433"),
		},
		Username:    "yugabyte_user",
		Password:    "yugabyte_pass",
		Database:    "yugabyte_db",
		LoadBalance: false,
	}
	e1 := "postgres://yugabyte_user:yugabyte_pass@localhost:5433/" +
		"yugabyte_db?sslmode=disable"
	connString, err := c1.DataSourceName()
	require.NoError(t, err)
	require.Equal(t, e1, connString)

	c2 := &DatabaseConfig{
		Endpoints: []*connection.Endpoint{
			test.MustCreateEndpoint("host1:1111"),
			test.MustCreateEndpoint("host2:2222"),
		},
		Username:    "yugabyte",
		Password:    "yugabyte",
		Database:    "yugabyte",
		LoadBalance: true,
	}
	//nolint:gosec // fake password.
	e2 := "postgres://yugabyte:yugabyte@host1:1111,host2:2222/yugabyte?sslmode=disable&load_balance=true"
	connString, err = c2.DataSourceName()
	require.NoError(t, err)
	require.Equal(t, e2, connString)
}
