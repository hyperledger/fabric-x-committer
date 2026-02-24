/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package testdb

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	docker "github.com/fsouza/go-dockerclient"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/utils"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

const (
	defaultStartTimeout = 5 * time.Minute
	defaultDBPrefix     = "sc_test_"

	deploymentLocal     = "local"
	deploymentContainer = "container"

	// YugaDBType represents the usage of Yugabyte DB.
	YugaDBType = "yugabyte"
	// PostgresDBType represents the usage of PostgreSQL DB.
	PostgresDBType = "postgres"

	yugaDBPort     = "5433"
	postgresDBPort = "5432"

	defaultLocalDBPort = "5433"

	deploymentTypeEnv = "DB_DEPLOYMENT"
	databaseTypeEnv   = "DB_TYPE"
)

// sharedContainer holds the container created by SetupSharedContainer for use
// by all tests.
//
// Write: SetupSharedContainer (called once from TestMain, before m.Run())
// Read:  StartAndConnect (called from individual tests, during m.Run())
// Write: CleanupSharedContainer (called once from TestMain, after m.Run())
//
// This is safe without synchronization because Go's test runner guarantees
// that TestMain completes SetupSharedContainer before any test goroutine
// starts, and CleanupSharedContainer runs after all test goroutines finish.
var sharedContainer *DatabaseContainer

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
	return PrepareTestEnvWithConnection(t, startAndConnect(ctx, t))
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
		if err := dropConn.execute(cleanUpCtx, fmt.Sprintf(dropDBSQLTempl, dbName)); err != nil {
			logger.Warnf("%+v", err)
		}
	})
	conn.Database = dbName
	return conn
}

// SetupSharedContainer creates a fresh test container for the current test.
// Call this from TestMain before m.Run(). The container uses a unique
// name to avoid interfering with developer containers or other test runs.
// For DB_DEPLOYMENT=local, this is a no-op (returns nil).
//
// Design decisions:
//   - Unique name (UUID suffix): never touches existing containers. A developer
//     may have a YugabyteDB container running for other purposes — we must not
//     stop, remove, or reuse it.
//   - Auto-assigned host port: avoids port conflicts with any other service on
//     the host, including other test runs.
//   - CleanupSharedContainer (called after m.Run) guarantees removal. In the
//     rare case of SIGKILL, `make kill-test-docker` cleans up all containers
//     matching the "sc_test" prefix.
func SetupSharedContainer() error {
	if getDBDeploymentFromEnv() != deploymentContainer {
		return nil
	}

	dc := &DatabaseContainer{
		DatabaseType: getDBTypeFromEnv(),
		Name:         fmt.Sprintf(defaultDBDeploymentTemplateName, getDBTypeFromEnv()) + "_" + uuid.NewString()[:8],
	}

	if err := dc.start(context.Background()); err != nil {
		return err
	}

	log.Printf("Started test container %s (image: %s)", dc.Name, dc.Image)
	sharedContainer = dc
	return nil
}

// startAndConnect returns a connection to the shared test container or a local
// DB instance. The shared container must have been created by
// SetupSharedContainer in TestMain before any test calls this function.
func startAndConnect(ctx context.Context, t *testing.T) *Connection {
	t.Helper()
	dbDeployment := getDBDeploymentFromEnv()

	var connOptions *Connection
	switch dbDeployment {
	case deploymentContainer:
		require.NotNil(t, sharedContainer)
		connOptions = sharedContainer.GetConnectionOptions(ctx, t)
	case deploymentLocal:
		connOptions = NewConnection(connection.CreateEndpointHP("localhost", defaultLocalDBPort))
	default:
		t.Logf("unknown db deployment type: %s", dbDeployment)
		return nil
	}
	t.Logf("connection endpoints: %+v", connOptions.Endpoints)
	return connOptions
}

// CleanupSharedContainer stops and removes the shared test container.
// Call this from TestMain after m.Run(). It is safe to call even when no
// container was created (e.g. DB_DEPLOYMENT=local).
func CleanupSharedContainer() {
	dc := sharedContainer
	if dc == nil {
		return
	}

	log.Printf("Stopping and removing test container %s", dc.Name)
	if err := dc.client.StopContainer(dc.ContainerID(), 10); err != nil {
		log.Printf("Warning: failed to stop container %s: %v", dc.Name, err)
	}

	if err := dc.client.RemoveContainer(docker.RemoveContainerOptions{
		ID:    dc.ContainerID(),
		Force: true,
	}); err != nil {
		log.Printf("Warning: failed to remove container %s: %v", dc.Name, err)
	}
}

// RunTestMain is a convenience wrapper for TestMain functions that only need
// the shared container lifecycle. It sets up the container, runs all tests,
// and cleans up. For TestMain functions with additional setup (e.g. building
// binaries), call SetupSharedContainer/CleanupSharedContainer directly.
func RunTestMain(m *testing.M) {
	if err := SetupSharedContainer(); err != nil {
		log.Printf("Failed to start shared container: %v", err)
	}
	defer CleanupSharedContainer()
	m.Run()
}
