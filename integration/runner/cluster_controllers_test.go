/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package runner

import (
	"context"
	"testing"
	"time"

	docker "github.com/fsouza/go-dockerclient"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/service/vc/dbtest"
)

func TestDBResiliencyYugabyteClusterController(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	t.Cleanup(cancel)

	cc, conn := StartYugaCluster(ctx, t, 3, 3)
	containersIDs := cc.GetNodesContainerID(t)

	t.Cleanup(func() {
		cc.stopAndRemoveCluster(t)
		ensureContainersRemoval(t, containersIDs)
	})

	dbtest.ConnectAndQueryTest(t, conn)
	require.Equal(t, 6, cc.GetClusterSize())

	cc.StopAndRemoveNodeWithRole(t, TabletNode)
	require.Equal(t, 5, cc.GetClusterSize())
}

func TestDBResiliencyPostgresClusterController(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	t.Cleanup(cancel)

	cc, conn := StartPostgresCluster(ctx, t)
	containersIDs := cc.GetNodesContainerID(t)

	t.Cleanup(func() {
		cc.stopAndRemoveCluster(t)
		ensureContainersRemoval(t, containersIDs)
	})

	dbtest.ConnectAndQueryTest(t, conn)
	require.Equal(t, 2, cc.GetClusterSize())

	cc.StopAndRemoveNodeWithRole(t, FollowerNode)
	require.Equal(t, 1, cc.GetClusterSize())
}

func ensureContainersRemoval(t *testing.T, containersIDs []string) {
	t.Helper()
	allContainers, err := dbtest.GetDockerClient(t).ListContainers(docker.ListContainersOptions{All: true})
	require.NoError(t, err)

	for _, c := range allContainers {
		for _, id := range containersIDs {
			require.NotEqual(t, c.ID, id)
		}
	}
}
