/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"context"
	_ "embed"
	"net"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/go-connections/nat"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/service/sidecar/sidecarclient"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	testutils "github.com/hyperledger/fabric-x-committer/utils/test"
)

const (
	sidecarPort        = "4001"
	loadGenMetricsPort = "2118"
)

// TestStartTestNode spawns an all-in-one instance of the committer using docker
// to verify that the committer container starts as expected.
func TestStartTestNode(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	stopAndRemoveContainersByName(ctx, t, createDockerClient(t), "committer")
	startCommitter(ctx, t, "committer")

	t.Log("Try to fetch the first block")
	sidecarEndpoint, err := connection.NewEndpoint(
		net.JoinHostPort("localhost", getContainerMappedHostPort(ctx, t, "committer", sidecarPort)),
	)
	require.NoError(t, err)
	committedBlock := sidecarclient.StartSidecarClient(ctx, t, &sidecarclient.Parameters{
		ChannelID: channelName,
		Client:    testutils.NewInsecureClientConfig(sidecarEndpoint),
	}, 0)
	b, ok := channel.NewReader(ctx, committedBlock).Read()
	require.True(t, ok)
	t.Logf("Received block #%d with %d TXs", b.Header.Number, len(b.Data.Data))

	monitorMetric(t, getContainerMappedHostPort(ctx, t, "committer", loadGenMetricsPort))
}

func startCommitter(ctx context.Context, t *testing.T, name string) {
	t.Helper()
	createAndStartContainerAndItsLogs(ctx, t, createAndStartContainerParameters{
		config: &container.Config{
			Image: testNodeImage,
			Cmd:   []string{"run", "db", "committer", "orderer", "loadgen"},
			ExposedPorts: nat.PortSet{
				sidecarPort + "/tcp":        struct{}{},
				loadGenMetricsPort + "/tcp": struct{}{},
			},
			Tty: true,
		},
		hostConfig: &container.HostConfig{
			NetworkMode: network.NetworkDefault,
			PortBindings: nat.PortMap{
				// sidecar port binding
				sidecarPort + "/tcp": []nat.PortBinding{{
					HostIP:   "localhost",
					HostPort: "0", // auto port assign
				}},
				// loadgen service port bindings
				loadGenMetricsPort + "/tcp": []nat.PortBinding{{
					HostIP:   "localhost",
					HostPort: "0", // auto port assign
				}},
			},
		},
		name: name,
	})
}
