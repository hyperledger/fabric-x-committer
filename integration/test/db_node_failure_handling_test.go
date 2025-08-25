/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"context"
	"testing"
	"time"

	"github.com/onsi/gomega"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
	"github.com/hyperledger/fabric-x-committer/integration/runner"
	"github.com/hyperledger/fabric-x-committer/service/vc/dbtest"
)

func TestDBResiliencyYugabyteScenarios(t *testing.T) {
	t.Parallel()

	for _, sc := range []struct {
		name   string
		action int
	}{
		{
			name:   "tablet",
			action: runner.Tablet,
		},
		{
			name:   "non-leader-master",
			action: runner.NonLeaderMaster,
		},
		{
			name:   "leader-master",
			action: runner.LeaderMaster,
		},
		{
			name:   "leader-master-and-tablet",
			action: runner.LeaderMaster | runner.Tablet,
		},
		{
			name:   "non-leader-master-and-tablet",
			action: runner.NonLeaderMaster | runner.Tablet,
		},
		{
			name:   "non-leader-master-and-tablet",
			action: runner.NonLeaderMaster | runner.LeaderMaster,
		},
	} {
		scenario := sc
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			clusterController, clusterConnection := runner.StartYugaCluster(createInitContext(t), t, 3, 3)
			c := registerAndCreateRuntime(t, clusterConnection)
			waitForCommittedTxs(t, c, 10_000)
			if scenario.action&runner.Tablet != 0 {
				clusterController.StopAndRemoveSingleNodeByRole(t, runner.TabletNode)
			}
			if scenario.action|runner.NonLeaderMaster != 0 {
				clusterController.StopAndRemoveMasterNode(t, runner.NonLeaderMaster)
			}
			if scenario.action&runner.LeaderMaster != 0 {
				clusterController.StopAndRemoveMasterNode(t, runner.LeaderMaster)
			}
			waitForCommittedTxs(t, c, 15_000)
		})
	}
}

func TestDBResiliencyPrimaryPostgresNodeCrash(t *testing.T) {
	t.Parallel()

	clusterController, clusterConnection := runner.StartPostgresCluster(createInitContext(t), t)

	c := registerAndCreateRuntime(t, clusterConnection)

	waitForCommittedTxs(t, c, 10_000)
	clusterController.StopAndRemoveSingleNodeByRole(t, runner.PrimaryNode)
	clusterController.PromoteSecondaryNode(t)
	waitForCommittedTxs(t, c, 15_000)
}

func TestDBResiliencySecondaryPostgresNodeCrash(t *testing.T) {
	t.Parallel()

	clusterController, clusterConnection := runner.StartPostgresCluster(createInitContext(t), t)

	c := registerAndCreateRuntime(t, clusterConnection)

	waitForCommittedTxs(t, c, 10_000)
	clusterController.StopAndRemoveSingleNodeByRole(t, runner.SecondaryNode)
	waitForCommittedTxs(t, c, 15_000)
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
