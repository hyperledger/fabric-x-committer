/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"testing"

	"github.com/cockroachdb/errors"
	docker "github.com/fsouza/go-dockerclient"
	"github.com/stretchr/testify/require"
)

// CreateDockerNetwork creates a network if it doesn't exist.
func CreateDockerNetwork(t *testing.T, name string) *docker.Network {
	t.Helper()
	client := GetDockerClient(t)
	network, err := client.NetworkInfo(name)
	if err == nil {
		t.Logf("network %s already exists", name)
		return network
	}

	network, err = client.CreateNetwork(docker.CreateNetworkOptions{
		Name:   name,
		Driver: "bridge",
	})
	require.NoError(t, err, "failed to create network")

	t.Logf("network %s created", network.Name)
	return network
}

// RemoveDockerNetwork removes a Docker network by name.
func RemoveDockerNetwork(t *testing.T, name string) {
	t.Helper()
	client := GetDockerClient(t)
	network, err := client.NetworkInfo(name)
	if errors.As(err, new(*docker.NoSuchNetwork)) {
		t.Logf("error: %s", err)
		return
	}
	require.NoError(t, err)

	if errors.As(client.RemoveNetwork(network.ID), new(*docker.NoSuchNetwork)) {
		t.Logf("error: %s", err)
		return
	}
	require.NoError(t, err)

	t.Logf("network %s removed successfully", name)
}

// GetDockerClient instantiate a new docker client.
func GetDockerClient(t *testing.T) *docker.Client {
	t.Helper()
	client, err := docker.NewClientFromEnv()
	require.NoError(t, err)
	return client
}
