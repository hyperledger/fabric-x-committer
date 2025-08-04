/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package dbtest

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/utils"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

const (
	defaultStartTimeout = 5 * time.Minute
	defaultDBPrefix     = "sc_test_"

	deploymentLocal     = "local"
	deploymentContainer = "container"

	YugaDBType     = "yugabyte" //nolint:revive
	PostgresDBType = "postgres"

	yugaDBPort     = "5433"
	postgresDBPort = "5432"

	defaultLocalDBPort = "5433"

	deploymentTypeEnv = "DB_DEPLOYMENT"
	databaseTypeEnv   = "DB_TYPE"
)

// randDbName generates random DB name.
// It digests the current time, the test name, and a random string to a base32 string.
func randDbName(t *testing.T) string {
	t.Helper()
	b := utils.MustRead(rand.Reader, 1024)
	b, err := time.Now().AppendBinary(b)
	require.NoError(t, err)
	s := sha256.New()
	s.Write([]byte(t.Name()))
	s.Write(b)
	uuidStr := strings.ToLower(strings.Trim(base32.StdEncoding.EncodeToString(s.Sum(nil)), "="))
	return defaultDBPrefix + uuidStr
}

// getDBDeploymentFromEnv get the desired DB deployment type from the environment variable.
func getDBDeploymentFromEnv() string {
	val, found := os.LookupEnv(deploymentTypeEnv)
	if found {
		return strings.ToLower(val)
	}

	return deploymentContainer
}

// getDBTypeFromEnv get the desired DB type from the environment variable.
func getDBTypeFromEnv() string {
	val, found := os.LookupEnv(databaseTypeEnv)
	if found {
		return strings.ToLower(val)
	}
	return YugaDBType
}

// PrepareTestEnv initializes a test environment for an existing or uncontrollable db instance.
func PrepareTestEnv(t *testing.T) *Connection {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), defaultStartTimeout)
	t.Cleanup(cancel)
	return PrepareTestEnvWithConnection(t, StartAndConnect(ctx, t))
}

// PrepareTestEnvWithConnection initializes a test environment given a db connection.
func PrepareTestEnvWithConnection(t *testing.T, conn *Connection) *Connection {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), defaultStartTimeout)
	t.Cleanup(cancel)
	require.True(t, conn.waitForReady(ctx), errors.Wrapf(ctx.Err(), "database is not ready"))
	t.Logf("connection nodes details: %s", conn.endpointsString())

	dbName := randDbName(t)
	t.Logf("[%s] db name: %s", t.Name(), dbName)
	require.NoError(t, conn.execute(ctx, fmt.Sprintf(createDBSQLTempl, dbName)))

	// we copy the connection for later usage.
	dropConn := *conn
	t.Cleanup(func() {
		//nolint:usetesting // t.Context is finishing right after the test resulting in context.Deadline error.
		cleanUpCtx, cleanUpCancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cleanUpCancel()
		logger.WarnStackTrace(dropConn.execute(cleanUpCtx, fmt.Sprintf(dropDBSQLTempl, dbName)))
	})
	conn.Database = dbName
	return conn
}

// StartAndConnect connects to an existing Yugabyte instance or creates a containerized new one.
func StartAndConnect(ctx context.Context, t *testing.T) *Connection {
	t.Helper()
	dbDeployment := getDBDeploymentFromEnv()

	var connOptions *Connection
	switch dbDeployment {
	case deploymentContainer:
		container := DatabaseContainer{
			DatabaseType: getDBTypeFromEnv(),
		}
		container.StartContainer(ctx, t)
		connOptions = container.getConnectionOptions(ctx, t)
	case deploymentLocal:
		connOptions = NewConnection(connection.CreateEndpointHP("localhost", defaultLocalDBPort))
	default:
		t.Logf("unknown db deployment type: %s", dbDeployment)
		return nil
	}
	t.Logf("connection endpoints: %+v", connOptions.Endpoints)
	return connOptions
}
