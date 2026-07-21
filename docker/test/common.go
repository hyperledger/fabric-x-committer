/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
	"github.com/hyperledger/fabric-x-committer/utils/test"
	"github.com/hyperledger/fabric-x-committer/utils/testdb"
)

const (
	// service names and commands.
	committerName   = "committer"
	ordererName     = "orderer"
	loadGenName     = "loadgen"
	sidecarName     = "sidecar"
	verifierName    = "verifier"
	vcName          = "vc"
	queryName       = "query"
	coordinatorName = "coordinator"
	dbName          = "db"

	runCMD        = "run"
	initDBCommand = "init-db"
)

type (
	createAndStartContainerParameters struct {
		config     *container.Config
		hostConfig *container.HostConfig
		name       string
	}
	startNodeParameters struct {
		node              string
		networkName       string
		tlsMode           string
		artifactsPath     string
		dbType            string
		dbPassword        string
		dbEndpointsString string
		dbInitTimeout     string
		cmd               []string
		additionalEnvs    []string
	}
)

func (p startNodeParameters) asNode(node string) startNodeParameters {
	params := p
	params.node = node
	return params
}

func (p startNodeParameters) dbUsername() string {
	if p.dbType == testdb.PostgresDBType {
		return "postgres"
	}
	return "yugabyte"
}

func (p startNodeParameters) dbDefaultDatabase() string {
	if p.dbType == testdb.PostgresDBType {
		return "postgres"
	}
	return "yugabyte"
}

const (
	monitoredMetric = "loadgen_transaction_committed_total"
	testNodeImage   = "docker.io/hyperledger/committer-test-node:latest"
	localhost       = "localhost"

	// containerArtifactsPath is the path to the artifacts directory inside the container.
	containerArtifactsPath = "/root/artifacts"
)

var localHostBind = []network.PortBinding{{
	HostIP:   netip.MustParseAddr("127.0.0.1"),
	HostPort: "0", // auto port assign
}}

func createAndStartContainerAndItsLogs(
	ctx context.Context,
	t *testing.T,
	params createAndStartContainerParameters,
) client.ContainerWaitResult {
	t.Helper()
	dockerClient := createDockerClient(t)
	resp, err := dockerClient.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name:       params.name,
		Config:     params.config,
		HostConfig: params.hostConfig,
	})
	require.NoError(t, err)
	// Subscribe before starting.
	// If the container finishes and is removed, before
	// ContainerWait is called, the container ID no longer exists and the exit status is lost.
	resultChannels := dockerClient.ContainerWait(ctx, resp.ID, client.ContainerWaitOptions{
		Condition: container.WaitConditionNextExit,
	})
	_, err = dockerClient.ContainerStart(ctx, resp.ID, client.ContainerStartOptions{})
	require.NoError(t, err)
	//nolint:contextcheck // We want to ensure cleanup when the test is done.
	t.Cleanup(func() {
		stopAndRemoveContainerByID(context.Background(), t, dockerClient, resp.ID)
	})
	logs, err := dockerClient.ContainerLogs(ctx, resp.ID, client.ContainerLogsOptions{
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
	return resultChannels
}

func monitorMetric(t *testing.T, metricsPort string, metricsTLS *connection.TLSConfig, waitForCount int) {
	t.Helper()
	tlsConf := test.MustGetTLSConfig(t, metricsTLS)

	metricsURL, err := monitoring.MakeMetricsURL(net.JoinHostPort(localhost, metricsPort), metricsTLS)
	require.NoError(t, err)

	currentNumberOfTxs := test.GetMetricValueFromURL(t, metricsURL, monitoredMetric, tlsConf)
	t.Logf("Check the load generator metrics from: %s", metricsURL)
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		committedTxs := test.GetMetricValueFromURL(t, metricsURL, monitoredMetric, tlsConf)
		require.Greater(ct, committedTxs, currentNumberOfTxs+waitForCount)
	}, 15*time.Minute, 100*time.Millisecond)
}

func stopAndRemoveContainersByName(ctx context.Context, t *testing.T, dockerClient *client.Client, names ...string) {
	t.Helper()
	list, err := dockerClient.ContainerList(ctx, client.ContainerListOptions{
		All: true,
	})
	require.NoError(t, err)

	nameToID := make(map[string]string)
	for _, c := range list.Items {
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
	_, err := dockerClient.ContainerStop(ctx, id, client.ContainerStopOptions{})
	if err != nil {
		t.Logf("unable to stop container %s: %s", id, err)
	}
	_, err = dockerClient.ContainerRemove(ctx, id, client.ContainerRemoveOptions{
		RemoveVolumes: true,
		Force:         true,
	})
	if err != nil {
		t.Logf("unable to remove container: %s", err)
	}
}

func getContainerMappedHostPort(
	ctx context.Context, t *testing.T, containerName string, containerPort network.Port,
) string {
	t.Helper()
	dockerClient := createDockerClient(t)
	defer connection.CloseConnectionsLog(dockerClient)
	info, err := dockerClient.ContainerInspect(ctx, containerName, client.ContainerInspectOptions{})
	require.NoError(t, err)
	require.NotNil(t, info)
	bindings, ok := info.Container.NetworkSettings.Ports[containerPort]
	require.True(t, ok)
	require.NotEmpty(t, bindings)
	return bindings[0].HostPort
}

func createDockerClient(t *testing.T) *client.Client {
	t.Helper()
	dockerClient, err := client.New(client.FromEnv)
	require.NoError(t, err)
	t.Cleanup(func() {
		connection.CloseConnectionsLog(dockerClient)
	})
	return dockerClient
}

func assembleContainerName(node, tlsMode, dbType string) string {
	return fmt.Sprintf("%s_%s_%s_%s", test.DockerNamesPrefix, node, tlsMode, dbType)
}

// waitForContainerHealthy waits until Docker reports the container as healthy.
func waitForContainerHealthy(ctx context.Context, t *testing.T, containerName string) {
	t.Helper()
	dockerClient := createDockerClient(t)
	defer connection.CloseConnectionsLog(dockerClient)
	t.Logf("Waiting for %s to be healthy", containerName)
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		inspect, err := dockerClient.ContainerInspect(ctx, containerName, client.ContainerInspectOptions{})
		assert.NoError(ct, err)
		if inspect.Container.State.Health != nil {
			assert.Equal(ct, container.Healthy, inspect.Container.State.Health.Status)
		}
	}, 3*time.Minute, 500*time.Millisecond, "%s did not become healthy", containerName)
	t.Logf("%s is healthy", containerName)
}

func copyArtifactsFromContainer(ctx context.Context, t *testing.T, containerName string) string {
	t.Helper()

	dockerClient := createDockerClient(t)
	defer connection.CloseConnectionsLog(dockerClient)
	res, err := dockerClient.CopyFromContainer(ctx, containerName, client.CopyFromContainerOptions{
		SourcePath: containerArtifactsPath,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, res.Content.Close())
	})

	artifactsPrefixSize := len(path.Base(containerArtifactsPath)) + 1

	hostDir := t.TempDir()
	tr := tar.NewReader(res.Content)
	for {
		header, tErr := tr.Next()
		if tErr == io.EOF {
			break
		}
		require.NoError(t, tErr)
		// Prevent directory traversal (Zip Slip) immediately after reading the entry.
		if strings.Contains(header.Name, "..") || strings.HasPrefix(header.Name, "/") {
			t.Fatalf("tar entry %q contains path traversal", header.Name)
		}
		target := filepath.Join(hostDir, header.Name[artifactsPrefixSize:])
		switch header.Typeflag {
		case tar.TypeDir:
			require.NoError(t, os.MkdirAll(target, os.FileMode(header.Mode))) //nolint:gosec // int64 > int32
		case tar.TypeReg:
			require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o750))
			//nolint:gosec // int64 > int32
			f, fErr := os.OpenFile(target, os.O_CREATE|os.O_WRONLY, os.FileMode(header.Mode))
			require.NoError(t, fErr)
			_, fErr = io.Copy(f, tr) //nolint:gosec
			require.NoError(t, fErr)
			require.NoError(t, f.Close())
		default:
		}
	}
	return hostDir
}
