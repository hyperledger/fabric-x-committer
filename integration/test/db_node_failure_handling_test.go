/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/integration/runner"
	"github.com/hyperledger/fabric-x-committer/service/vc/dbtest"
)

const (
	linuxOS = "linux"
	// Tablet is a tablet (data node) in the cluster.
	Tablet = 1 << iota
	// NonLeaderMaster is a master node that does not hold the leader role.
	NonLeaderMaster
	// LeaderMaster is the master node currently acting as the leader.
	LeaderMaster
)

func TestDBResiliencyYugabyteScenarios(t *testing.T) {
	// Parallel is only enabled on Linux. On macOS each subtest spins up a
	// 6-container YugabyteDB cluster (3 masters + 3 tablets). Running multiple
	// clusters concurrently exhausts Docker Desktop resources and causes
	// container startup timeouts.
	if runtime.GOOS == linuxOS {
		t.Parallel()
	}

	for _, sc := range []struct {
		name   string
		action int
	}{
		{
			name:   "tablet",
			action: Tablet,
		},
		{
			name:   "non-leader-master",
			action: NonLeaderMaster,
		},
		{
			name:   "leader-master",
			action: LeaderMaster,
		},
		{
			name:   "leader-master-and-tablet",
			action: LeaderMaster | Tablet,
		},
		{
			name:   "non-leader-master-and-tablet",
			action: NonLeaderMaster | Tablet,
		},
	} {
		scenario := sc
		t.Run(scenario.name, func(t *testing.T) {
			if runtime.GOOS == linuxOS {
				t.Parallel()
			}
			clusterController, clusterConnection := runner.StartYugaCluster(createInitContext(t), t, 3, 3)
			c := registerAndCreateRuntime(t, clusterConnection)
			waitForCommittedTxs(t, c, 10_000)
			if scenario.action&Tablet != 0 {
				clusterController.StopAndRemoveSingleNodeByRole(t, runner.TabletNode)
			}
			if scenario.action&NonLeaderMaster != 0 {
				clusterController.StopAndRemoveSingleMasterNodeByRaftRole(t, runner.FollowerMasterNode)
			}
			if scenario.action&LeaderMaster != 0 {
				clusterController.StopAndRemoveSingleMasterNodeByRaftRole(t, runner.LeaderMasterNode)
			}
			waitForCommittedTxs(t, c, 15_000)
		})
	}
}

func TestDBResiliencyPrimaryPostgresNodeCrash(t *testing.T) {
	// See TestDBResiliencyYugabyteScenarios for why parallel is Linux-only.
	if runtime.GOOS == linuxOS {
		t.Parallel()
	}

	clusterController, clusterConnection := runner.StartPostgresCluster(createInitContext(t), t)

	c := registerAndCreateRuntime(t, clusterConnection)

	waitForCommittedTxs(t, c, 10_000)
	clusterController.StopAndRemoveSingleNodeByRole(t, runner.PrimaryNode)
	clusterController.PromoteSecondaryNode(t)
	waitForCommittedTxs(t, c, 15_000)
}

func TestDBResiliencySecondaryPostgresNodeCrash(t *testing.T) {
	// See TestDBResiliencyYugabyteScenarios for why parallel is Linux-only.
	if runtime.GOOS == linuxOS {
		t.Parallel()
	}

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
		BlockTimeout: 2 * time.Second,
		BlockSize:    500,
		DBConnection: clusterConnection,
	})
	c.Start(t, runner.FullTxPathWithLoadGen)

	return c
}

func waitForCommittedTxs(t *testing.T, c *runner.CommitterRuntime, waitForCount int) {
	t.Helper()
	currentNumberOfTxs := c.CountStatus(t, committerpb.Status_COMMITTED)
	require.Eventually(t,
		func() bool {
			committedTxs := c.CountStatus(t, committerpb.Status_COMMITTED)
			t.Logf("Amount of committed txs: %d\n", committedTxs)
			return committedTxs > currentNumberOfTxs+waitForCount
		},
		120*time.Second,
		500*time.Millisecond,
	)
	require.Zero(t, c.CountAlternateStatus(t, committerpb.Status_COMMITTED))
}

// createInitContext returns a context with a timeout covering the full cluster
// lifecycle: container image pulls, sequential startup of 6 nodes (on macOS),
// transaction processing, node removal, and recovery verification.
func createInitContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	t.Cleanup(cancel)

	return ctx
}
