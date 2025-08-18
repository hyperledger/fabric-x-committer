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

type yugabyteScenario struct {
	name   string
	action int
}

const (
	TABLET            = 1 << iota
	NON_LEADER_MASTER //nolint:revive
	LEADER_MASTER     //nolint:revive
)

var crashScenarios = []yugabyteScenario{
	{
		name:   "tablet",
		action: TABLET,
	},
	{
		name:   "non-leader-master",
		action: NON_LEADER_MASTER,
	},
	{
		name:   "leader-master",
		action: LEADER_MASTER,
	},
	{
		name:   "leader-master-and-tablet",
		action: LEADER_MASTER | TABLET,
	},
	{
		name:   "non-leader-master-and-tablet",
		action: NON_LEADER_MASTER | TABLET,
	},
}

func TestDBResiliencyYugabyteScenarios(t *testing.T) {
	t.Parallel()

	for _, sc := range crashScenarios {
		scenario := sc
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			runYBScenario(t, scenario.action)
		})
	}
}

func TestDBResiliencyPrimaryPostgresNodeCrash(t *testing.T) {
	t.Parallel()

	clusterController, clusterConnection := runner.StartPostgresCluster(createInitContext(t), t)

	c := registerAndCreateRuntime(t, clusterConnection)

	waitForCommittedTxs(t, c, 10_000)
	clusterController.StopAndRemoveSingleNodeWithRole(t, runner.PrimaryNode)
	clusterController.PromoteSecondaryNode(t)
	waitForCommittedTxs(t, c, 15_000)
}

func TestDBResiliencySecondaryPostgresNodeCrash(t *testing.T) {
	t.Parallel()

	clusterController, clusterConnection := runner.StartPostgresCluster(createInitContext(t), t)

	c := registerAndCreateRuntime(t, clusterConnection)

	waitForCommittedTxs(t, c, 10_000)
	clusterController.StopAndRemoveSingleNodeWithRole(t, runner.SecondaryNode)
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

func executeAction(t *testing.T, action int, cc *runner.YugaClusterController) {
	t.Helper()

	if action&TABLET != 0 {
		cc.StopAndRemoveSingleNodeWithRole(t, runner.TabletNode)
	}

	if action&LEADER_MASTER != 0 {
		cc.RemoveLeaderMasterNode(t)
	}

	if action&NON_LEADER_MASTER != 0 {
		cc.RemoveNonLeaderMasterNode(t)
	}
}

func runYBScenario(t *testing.T, action int) {
	t.Helper()

	clusterController, clusterConnection := runner.StartYugaCluster(createInitContext(t), t, 3, 3)
	c := registerAndCreateRuntime(t, clusterConnection)

	waitForCommittedTxs(t, c, 10_000)
	executeAction(t, action, clusterController)
	waitForCommittedTxs(t, c, 15_000)
}
