/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

type (
	createAndStartContainerParameters struct {
		config     *container.Config
		hostConfig *container.HostConfig
		name       string
	}
	startNodeParameters struct {
		credsFactory    *test.CredentialsFactory
		node            string
		networkName     string
		tlsMode         string
		configBlockPath string
		dbType          string
		dbPassword      string
		cmd             []string
	}
)

func (p *startNodeParameters) asNode(node string) startNodeParameters {
	params := *p
	params.node = node
	return params
}

const (
	channelName         = "mychannel"
	monitoredMetric     = "loadgen_transaction_committed_total"
	containerPrefixName = "sc_test"
	testNodeImage       = "docker.io/hyperledger/committer-test-node:latest"
)

func createAndStartContainerAndItsLogs(
	ctx context.Context,
	t *testing.T,
	params createAndStartContainerParameters,
) {
	t.Helper()
	dockerClient := createDockerClient(t)
	resp, err := dockerClient.ContainerCreate(
		ctx, params.config, params.hostConfig, nil, nil, params.name,
	)
	require.NoError(t, err)
	require.NoError(t, dockerClient.ContainerStart(ctx, resp.ID, container.StartOptions{}))

	//nolint:contextcheck // We want to ensure cleanup when the test is done.
	t.Cleanup(func() {
		stopAndRemoveContainerByID(context.Background(), t, dockerClient, resp.ID)
	})

	logs, err := dockerClient.ContainerLogs(ctx, resp.ID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
	})
	require.NoError(t, err)
	go func() {
		_, err = io.Copy(os.Stdout, logs)
		if err != nil {
			t.Logf("[%s] logs ended with: %v", params.name, err)
		}
	}()
}

func monitorMetric(t *testing.T, metricsPort string) {
	t.Helper()
	metricsURL, err := monitoring.MakeMetricsURL(net.JoinHostPort("localhost", metricsPort))
	require.NoError(t, err)

	t.Logf("Check the load generator metrics from: %s", metricsURL)
	// We check often since the load generator's metrics might be closed if the limit is reached.
	// We log only if there are changes to avoid spamming the log.
	prevCount := -1
	require.Eventually(t, func() bool {
		count := test.GetMetricValueFromURL(t, metricsURL, monitoredMetric)
		if prevCount != count {
			t.Logf("%s: %d", monitoredMetric, count)
		}
		prevCount = count
		return count > 1_000
	}, 15*time.Minute, 100*time.Millisecond)
}

func stopAndRemoveContainersByName(ctx context.Context, t *testing.T, dockerClient *client.Client, names ...string) {
	t.Helper()
	list, err := dockerClient.ContainerList(ctx, container.ListOptions{
		All: true,
	})
	require.NoError(t, err)

	nameToID := make(map[string]string)
	for _, c := range list {
		for _, name := range c.Names {
			nameToID[name[1:]] = c.ID
		}
	}
	for _, containerName := range names {
		id, ok := nameToID[containerName]
		if !ok {
			t.Logf("container '%s' not found", containerName)
			continue
		}
		t.Logf("stopping container '%s' (%s)", containerName, id)
		stopAndRemoveContainerByID(ctx, t, dockerClient, id)
	}
}

func stopAndRemoveContainerByID(ctx context.Context, t *testing.T, dockerClient *client.Client, id string) {
	t.Helper()
	err := dockerClient.ContainerStop(ctx, id, container.StopOptions{})
	if err != nil {
		t.Logf("unable to stop container %s: %s", id, err)
	}
	err = dockerClient.ContainerRemove(ctx, id, container.RemoveOptions{
		RemoveVolumes: true,
		Force:         true,
	})
	if err != nil {
		t.Logf("unable to remove container: %s", err)
	}
}

func getContainerMappedHostPort(
	ctx context.Context, t *testing.T, containerName, containerPort string,
) string {
	t.Helper()
	info, err := createDockerClient(t).ContainerInspect(ctx, containerName)
	require.NoError(t, err)
	require.NotNil(t, info)
	portKey := nat.Port(fmt.Sprintf("%s/tcp", containerPort))
	bindings, ok := info.NetworkSettings.Ports[portKey]
	require.True(t, ok)
	require.NotEmpty(t, bindings)
	return bindings[0].HostPort
}

func createDockerClient(t *testing.T) *client.Client {
	t.Helper()
	dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	require.NoError(t, err)
	defer connection.CloseConnectionsLog(dockerClient)
	return dockerClient
}

func assembleBinds(t *testing.T, params startNodeParameters, additionalBinds ...string) []string {
	t.Helper()

	_, serverCredsPath := params.credsFactory.CreateServerCredentials(t, params.tlsMode, params.node)
	require.NotEmpty(t, serverCredsPath)
	_, clientCredsPath := params.credsFactory.CreateClientCredentials(t, params.tlsMode)
	require.NotEmpty(t, clientCredsPath)

	return append([]string{
		fmt.Sprintf("%s:/server-certs", serverCredsPath),
		fmt.Sprintf("%s:/client-certs", clientCredsPath),
	}, additionalBinds...)
}

func assembleContainerName(node, tlsMode, dbType string) string {
	return fmt.Sprintf("%s_%s_%s_%s", test.DockerNamesPrefix, node, tlsMode, dbType)
}
