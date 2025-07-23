/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"context"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"testing"
	"time"

	"github.com/onsi/gomega"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
	"github.com/hyperledger/fabric-x-committer/integration/runner"
	"github.com/hyperledger/fabric-x-committer/service/vc/dbtest"
)

func TestDBResiliencyYugabyteFollowerNodeCrash(t *testing.T) {
	t.Parallel()

	clusterController, clusterConnection := runner.StartYugaCluster(createInitContext(t), t, 3)

	c := registerAndCreateRuntime(t, clusterConnection)

	waitForCommittedTxs(t, c, 10_000)
	clusterController.StopAndRemoveNodeWithRole(t, runner.FollowerNode)
	waitForCommittedTxs(t, c, 15_000)
}

func TestDBResiliencyYugabyteLeaderNodeCrash(t *testing.T) {
	t.Parallel()

	clusterController, clusterConnection := runner.StartYugaCluster(createInitContext(t), t, 3)

	// Yugabyte has an undetected bug that causes slow tablet distribution.
	// As a result, when a leader node fails before replication is complete,
	// the db hangs and the remaining nodes become unreachable.
	// To support this test, wait for the tablet replication to finish (~3m).
	time.Sleep(3 * time.Minute)

	c := registerAndCreateRuntime(t, clusterConnection)

	waitForCommittedTxs(t, c, 10_000)
	clusterController.StopAndRemoveNodeWithRole(t, runner.LeaderNode)
	waitForCommittedTxs(t, c, 15_000)
}

func TestDBResiliencyPrimaryPostgresNodeCrash(t *testing.T) {
	t.Parallel()

	clusterController, clusterConnection := runner.StartPostgresCluster(createInitContext(t), t)

	c := registerAndCreateRuntime(t, clusterConnection)

	waitForCommittedTxs(t, c, 10_000)
	clusterController.StopAndRemoveNodeWithRole(t, runner.LeaderNode)
	clusterController.PromoteFollowerNode(t)
	waitForCommittedTxs(t, c, 15_000)
}

func TestNothing(t *testing.T) {
	dbtest.GetDockerContainersIPs(t)
}

func TestDBResiliencySecondaryPostgresNodeCrash(t *testing.T) {
	t.Parallel()

	//clusterController, clusterConnection := runner.StartPostgresCluster(createInitContext(t), t)

	clusterConnection := dbtest.NewConnection([]*connection.Endpoint{
		connection.CreateEndpointHP("172.19.0.3", "5433"),
		connection.CreateEndpointHP("172.19.0.2", "5433"),
		connection.CreateEndpointHP("172.19.0.4", "5433"),
	}...)

	c := registerAndCreateRuntime(t, clusterConnection)

	waitForCommittedTxs(t, c, 50_000)
	//clusterController.StopAndRemoveNodeWithRole(t, runner.FollowerNode)
	//waitForCommittedTxs(t, c, 15_000)
}

func registerAndCreateRuntime(t *testing.T, clusterConnection *dbtest.Connection) *runner.CommitterRuntime {
	t.Helper()
	gomega.RegisterTestingT(t)
	c := runner.NewRuntime(t, &runner.Config{
		NumVerifiers: 2,
		NumVCService: 2,
		BlockTimeout: 2 * time.Second,
		BlockSize:    500,
		DBCluster:    clusterConnection,
	})
	c.Start(t, runner.FullTxPathWithLoadGen)

	return c
}

func waitForCommittedTxs(t *testing.T, c *runner.CommitterRuntime, waitForCount int) {
	t.Helper()
	currentNumberOfTxs := c.CountStatus(t, protoblocktx.Status_COMMITTED)
	require.Eventually(t,
		func() bool {
			committedTxs := c.CountStatus(t, protoblocktx.Status_COMMITTED)
			t.Logf("Amount of committed txs: %d\n", committedTxs)
			return committedTxs > currentNumberOfTxs+waitForCount
		},
		90*time.Second,
		500*time.Millisecond,
	)
	require.Zero(t, c.CountAlternateStatus(t, protoblocktx.Status_COMMITTED))
}

func createInitContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	t.Cleanup(cancel)

	return ctx
}
