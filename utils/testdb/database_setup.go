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
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	docker "github.com/fsouza/go-dockerclient"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/utils"
	"github.com/hyperledger/fabric-x-committer/utils/test"
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

	// localYugaContainerEnv overrides the name of the locally-started YugabyteDB
	// container that DB_DEPLOYMENT=local tests exec yb-admin into (for snapshot
	// schedule creation). Defaults to the name used by scripts/get-and-start-yuga.sh.
	localYugaContainerEnv     = "YUGA_CONTAINER"
	defaultLocalYugaContainer = "sc_test_yugabyte"
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
		connOptions = NewConnection(getDBTypeFromEnv(), test.NewEndpoint(t, "localhost", defaultLocalDBPort))
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

// EnsureSnapshotSchedule creates a YugabyteDB snapshot schedule for dbName.
// This is opt-in and called only by tests that create a snapshot database. It is
// a no-op for postgres.
//
// Deployment modes:
//   - container: exec yb-admin via the docker client on the container object.
//   - local (e.g. CI): exec yb-admin through `docker exec <YUGA_CONTAINER>`.
func EnsureSnapshotSchedule(t *testing.T, dbName string) {
	t.Helper()
	if getDBTypeFromEnv() != YugaDBType {
		return
	}
	if sharedContainer != nil {
		sharedContainer.EnsureSnapshotSchedule(t, dbName)
		return
	}
	if getDBDeploymentFromEnv() == deploymentLocal {
		ensureLocalSnapshotSchedule(t, dbName)
	}
}

// ensureLocalSnapshotSchedule creates (and registers cleanup for) a snapshot schedule
// on the locally-started YugabyteDB container by running yb-admin through `docker exec`.
// The container name comes from YUGA_CONTAINER (default: the name used by
// scripts/get-and-start-yuga.sh). That script starts yugabyted with
// --advertise_address 0.0.0.0, so yb-master is reachable at the default 127.0.0.1:7100
// from inside the container.
func ensureLocalSnapshotSchedule(t *testing.T, dbName string) {
	t.Helper()
	container := os.Getenv(localYugaContainerEnv)
	if container == "" {
		container = defaultLocalYugaContainer
	}
	const masterAddr = "127.0.0.1:7100"
	t.Logf("creating snapshot schedule for ysql.%s on container %s", dbName, container)

	output := dockerExec(t, container, createSnapshotScheduleCmd(masterAddr, dbName)...)
	scheduleID := parseSnapshotScheduleID(t, output)

	t.Cleanup(func() {
		dockerExec(t, container, deleteSnapshotScheduleCmd(masterAddr, scheduleID)...)
	})
}

// createSnapshotScheduleCmd builds the yb-admin argv that creates a snapshot schedule
// (interval 1 minute, retention 10 minutes) for the ysql.<dbName> keyspace.
func createSnapshotScheduleCmd(masterAddr, dbName string) []string {
	return []string{
		"bin/yb-admin", "--master_addresses", masterAddr,
		"create_snapshot_schedule", "1", "10", "ysql." + dbName,
	}
}

// deleteSnapshotScheduleCmd builds the yb-admin argv that deletes a snapshot schedule.
func deleteSnapshotScheduleCmd(masterAddr, scheduleID string) []string {
	return []string{
		"bin/yb-admin", "--master_addresses", masterAddr,
		"delete_snapshot_schedule", scheduleID,
	}
}

// parseSnapshotScheduleID extracts the schedule_id from yb-admin create_snapshot_schedule
// JSON output, failing the test if it is missing.
func parseSnapshotScheduleID(t *testing.T, output string) string {
	t.Helper()
	var resp struct {
		ScheduleID string `json:"schedule_id"`
	}
	require.NoError(t, json.Unmarshal([]byte(output), &resp),
		"unexpected create_snapshot_schedule output: %s", output)
	require.NotEmpty(t, resp.ScheduleID, "create_snapshot_schedule returned no schedule_id: %s", output)
	return resp.ScheduleID
}

// dockerExec runs `docker exec <container> <args...>` and returns stdout, failing the
// test on a non-zero exit.
func dockerExec(t *testing.T, container string, args ...string) string {
	t.Helper()
	//nolint:gosec // test-only, fixed args.
	cmd := exec.Command("docker", append([]string{"exec", container}, args...)...)
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "docker exec %s %s failed: %s", container, strings.Join(args, " "), out)
	return string(out)
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
