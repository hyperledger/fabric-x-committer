/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package dbtest

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	docker "github.com/fsouza/go-dockerclient"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

const (
	defaultYugabyteImage            = "yugabytedb/yugabyte:2.20.7.0-b58"
	defaultPostgresImage            = "postgres:16.9-alpine3.21"
	defaultDBDeploymentTemplateName = test.DockerNamesPrefix + "_%s_unit_tests"

	// container's Memory and CPU management.
	gb         = 1 << 30 // gb is the number of bytes needed to represent 1 GB.
	memorySwap = -1      // memorySwap disable memory swaps (don't store data on disk)

	// YugabytedReadinessOutput is the output indicating that a Yugabyted node is ready.
	YugabytedReadinessOutput = "Data placement constraint successfully verified"
	// YugabyteTabletNodeReadinessOutput is the output indicating that a yugabyte's tablet node is ready.
	YugabyteTabletNodeReadinessOutput = "syncing data to disk ... ok"
	// PostgresReadinesssOutput is the output indicating that a PostgreSQL node is ready.
	PostgresReadinesssOutput = "database system is ready to accept connections"
	// SecondaryPostgresNodeReadinessOutput is the output indicating that a secondary PostgreSQL node is ready.
	SecondaryPostgresNodeReadinessOutput = "started streaming WAL from primary"

	// Represents the required database TLS certificate files name.
	yugabytePublicKeyFileName     = "node.db.crt"
	yugabytePrivateKeyFileName    = "node.db.key"
	postgresPublicKeyFileName     = "server.crt"
	postgresPrivateKeyFileName    = "server.key"
	yugabyteCACertificateFileName = "ca.crt"
)

var (
	// YugabyteCMD starts yugabyte without fault tolerance (single server).
	YugabyteCMD = []string{
		"bin/yugabyted", "start",
		"--callhome", "false",
		"--background", "false",
		"--ui", "false",
		"--tserver_flags",
		"ysql_max_connections=500," +
			"tablet_replicas_per_gib_limit=4000," +
			"yb_num_shards_per_tserver=1," +
			"minloglevel=3," +
			"yb_enable_read_committed_isolation=true",
	}

	// passwordRegex is the compiled regular expression.
	// to efficiently extract the password of the Yugabyted node.
	passwordRegex = regexp.MustCompile(`(?i)(?m)^password:\s*(.+)$`)
)

// DatabaseContainer manages the execution of an instance of a dockerized DB for tests.
type DatabaseContainer struct {
	Name         string
	Image        string
	HostIP       string
	Network      string
	Hostname     string
	DatabaseType string
	Tag          string
	Role         string
	Cmd          []string
	Env          []string
	Binds        []string
	HostPort     int
	DbPort       docker.Port
	PortMap      docker.Port
	PortBinds    map[docker.Port][]docker.PortBinding
	NetToIP      map[string]*docker.EndpointConfig
	AutoRm       bool
	// TLSConfig holds the node TLS certificates.
	// If TLSConfig isn't available (is nil), we fallback to insecure mode.
	TLSConfig *connection.TLSConfig

	client      *docker.Client
	containerID string
}

// StartContainer runs a DB container, if no specific container details provided, default values will be set.
func (dc *DatabaseContainer) StartContainer(ctx context.Context, t *testing.T) {
	t.Helper()

	dc.initDefaults(t)

	dc.createContainer(ctx, t)

	// Starts the container
	err := dc.client.StartContainerWithContext(dc.containerID, nil, ctx)
	var containerAlreadyRunning *docker.ContainerAlreadyRunning
	if errors.As(err, &containerAlreadyRunning) {
		t.Log("Container is already running")
		return
	}
	require.NoError(t, err)

	// Stream logs to stdout/stderr
	go dc.streamLogs(t)
}

func (dc *DatabaseContainer) initDefaults(t *testing.T) { //nolint:gocognit
	t.Helper()

	switch dc.DatabaseType {
	case YugaDBType:
		if dc.Image == "" {
			dc.Image = defaultYugabyteImage
		}

		if dc.Cmd == nil {
			dc.Cmd = YugabyteCMD
			if dc.TLSConfig != nil {
				require.NotEmpty(t, dc.Hostname)
				dc.Cmd = append(dc.Cmd, "--secure", "--certs_dir=/creds", "--advertise_address", dc.Hostname)
			} else {
				dc.Cmd = append(dc.Cmd, "--insecure")
			}
		}

		if dc.DbPort == "" {
			dc.DbPort = docker.Port(fmt.Sprintf("%s/tcp", yugaDBPort))
		}

		if dc.TLSConfig != nil {
			require.NotEmpty(t, dc.TLSConfig.CACertPaths)
			dc.Binds = append(dc.Binds,
				fmt.Sprintf("%s:/creds/%s", dc.TLSConfig.CertPath, yugabytePublicKeyFileName),
				fmt.Sprintf("%s:/creds/%s", dc.TLSConfig.KeyPath, yugabytePrivateKeyFileName),
				fmt.Sprintf("%s:/creds/%s", dc.TLSConfig.CACertPaths[0], yugabyteCACertificateFileName),
			)
		}
	case PostgresDBType:
		if dc.Image == "" {
			dc.Image = defaultPostgresImage
		}

		if dc.Env == nil {
			dc.Env = []string{
				"POSTGRES_PASSWORD=yugabyte",
				"POSTGRES_USER=yugabyte",
			}
		}

		if dc.DbPort == "" {
			dc.DbPort = docker.Port(fmt.Sprintf("%s/tcp", postgresDBPort))
		}

		if dc.TLSConfig != nil {
			dc.Binds = append(dc.Binds,
				fmt.Sprintf("%s:/creds/%s", dc.TLSConfig.CertPath, postgresPublicKeyFileName),
				fmt.Sprintf("%s:/creds/%s", dc.TLSConfig.KeyPath, postgresPrivateKeyFileName),
			)
			dc.Cmd = []string{
				// Configure PostgreSQL to run in secure mode
				// and listen for incoming connections on a chosen port.
				"-c", fmt.Sprintf("port=%s", dc.DbPort),
				"-c", "ssl=on",
				"-c", fmt.Sprintf("ssl_cert_file=%s", filepath.Join("/creds", postgresPublicKeyFileName)),
				"-c", fmt.Sprintf("ssl_key_file=%s", filepath.Join("/creds", postgresPrivateKeyFileName)),
			}
		}
	default:
		t.Fatalf("Unsupported database type: %s", dc.DatabaseType)
	}

	if dc.Name == "" {
		dc.Name = fmt.Sprintf(defaultDBDeploymentTemplateName, dc.DatabaseType)
		if dc.TLSConfig != nil {
			dc.Name += fmt.Sprintf("_with_tls_%s", uuid.NewString()[0:8])
		}
	}

	if dc.client == nil {
		dc.client = test.GetDockerClient(t)
	}
}

// createContainer attempts to create a container instance, or attach to an existing one.
func (dc *DatabaseContainer) createContainer(ctx context.Context, t *testing.T) {
	t.Helper()
	// If container exists, we don't have to create it.
	err := dc.findContainer(t)
	if err == nil {
		return
	}

	// Pull the image if not exist
	require.NoError(t, dc.client.PullImage(docker.PullImageOptions{
		Context:      ctx,
		Repository:   dc.Image,
		Tag:          dc.Tag,
		OutputStream: os.Stdout,
	}, docker.AuthConfiguration{}))

	// Create the container instance
	container, err := dc.client.CreateContainer(
		docker.CreateContainerOptions{
			Context: ctx,
			Name:    dc.Name,
			Config: &docker.Config{
				Image:    dc.Image,
				Cmd:      dc.Cmd,
				Env:      dc.Env,
				Hostname: dc.Hostname,
			},
			HostConfig: &docker.HostConfig{
				AutoRemove:   dc.AutoRm,
				PortBindings: dc.PortBinds,
				NetworkMode:  dc.Network,
				Binds:        dc.Binds,
				Memory:       4 * gb,
				MemorySwap:   memorySwap,
			},
		},
	)

	// If container created successfully, finish.
	if err == nil {
		dc.containerID = container.ID
		return
	}
	require.ErrorIs(t, err, docker.ErrContainerAlreadyExists)

	// Try to find it again.
	require.NoError(t, dc.findContainer(t))
}

// findContainer looks up a container with the same name.
func (dc *DatabaseContainer) findContainer(t *testing.T) error {
	t.Helper()
	allContainers, err := dc.client.ListContainers(docker.ListContainersOptions{All: true})
	require.NoError(t, err, "could not load containers.")

	names := make([]string, 0, len(allContainers))
	for _, c := range allContainers {
		for _, n := range c.Names {
			names = append(names, n)
			if n == dc.Name || n == fmt.Sprintf("/%s", dc.Name) {
				dc.containerID = c.ID
				return nil
			}
		}
	}
	return errors.Errorf("cannot find container '%s'. Containers: %v", dc.Name, names)
}

// GetConnectionOptions inspect the container and fetches the available connection options.
func (dc *DatabaseContainer) GetConnectionOptions(ctx context.Context, t *testing.T) *Connection {
	t.Helper()
	container, err := dc.client.InspectContainerWithOptions(docker.InspectContainerOptions{
		Context: ctx,
		ID:      dc.containerID,
	})
	require.NoError(t, err)

	endpoints := []*connection.Endpoint{
		dc.GetContainerConnectionDetails(t),
	}
	for _, p := range container.NetworkSettings.Ports[dc.DbPort] {
		endpoints = append(endpoints, connection.CreateEndpointHP(p.HostIP, p.HostPort))
	}

	return NewConnection(endpoints...)
}

// GetContainerConnectionDetails inspect the container and fetches its connection to an endpoint.
func (dc *DatabaseContainer) GetContainerConnectionDetails(
	t *testing.T,
) *connection.Endpoint {
	t.Helper()
	container, err := dc.client.InspectContainerWithOptions(docker.InspectContainerOptions{
		Context: t.Context(),
		ID:      dc.containerID,
	})
	require.NoError(t, err)

	ipAddress := container.NetworkSettings.IPAddress
	require.NotNil(t, ipAddress)
	if dc.Network != "" {
		net, ok := container.NetworkSettings.Networks[dc.Network]
		require.True(t, ok)
		ipAddress = net.IPAddress
	}
	return connection.CreateEndpointHP(ipAddress, dc.DbPort.Port())
}

// streamLogs streams the container output to the requested stream.
func (dc *DatabaseContainer) streamLogs(t *testing.T) {
	t.Helper()
	logOptions := docker.LogsOptions{
		//nolint:usetesting //t.Context finished after the function call, which is causing an unexpected crash.
		Context:      context.Background(),
		Container:    dc.containerID,
		Follow:       true,
		ErrorStream:  os.Stderr,
		OutputStream: os.Stdout,
		Stderr:       true,
		Stdout:       true,
	}

	assert.NoError(t, dc.client.Logs(logOptions))
}

// GetContainerLogs return the output of the DatabaseContainer.
func (dc *DatabaseContainer) GetContainerLogs(t *testing.T) string {
	t.Helper()
	var outputBuffer bytes.Buffer
	require.NoError(t, dc.client.Logs(docker.LogsOptions{
		Stdout:       true,
		Stderr:       true,
		Container:    dc.Name,
		OutputStream: &outputBuffer,
		ErrorStream:  &outputBuffer,
	}))

	return outputBuffer.String()
}

// StopAndRemoveContainer stops and removes the db container from the docker engine.
func (dc *DatabaseContainer) StopAndRemoveContainer(t *testing.T) {
	t.Helper()
	dc.StopContainer(t)
	require.NoError(t, dc.client.RemoveContainer(docker.RemoveContainerOptions{
		ID:    dc.ContainerID(),
		Force: true,
	}))
	t.Logf("Container %s stopped and removed successfully", dc.ContainerID())
}

// StopContainer stops db container.
func (dc *DatabaseContainer) StopContainer(t *testing.T) {
	t.Helper()
	require.NoError(t, dc.client.StopContainer(dc.ContainerID(), 10))
}

// ContainerID returns the container ID.
func (dc *DatabaseContainer) ContainerID() string {
	return dc.containerID
}

// ReadPasswordFromContainer extracts the randomly generated password from a file inside the container.
// This is required because YugabyteDB, when running in secure mode, doesn't allow default passwords
// and instead generates a random one at startup.
// This method being called only when a secured Yugabyted node is started.
// If the file doesn’t exist or doesn't contain a password, the test should fail.
func (dc *DatabaseContainer) ReadPasswordFromContainer(t *testing.T, filePath string) string {
	t.Helper()
	output := dc.ExecuteCommand(t, []string{"cat", filePath})
	found := passwordRegex.FindStringSubmatch(output)
	require.Greater(t, len(found), 1)
	return found[1]
}

// ExecuteCommand executes a command and returns the container output.
func (dc *DatabaseContainer) ExecuteCommand(t *testing.T, cmd []string) string {
	t.Helper()
	require.NotNil(t, dc.client)
	t.Logf("executing %s", strings.Join(cmd, " "))

	var stdout strings.Builder
	var stderr strings.Builder
	exec, err := dc.client.CreateExec(docker.CreateExecOptions{
		Container:    dc.containerID,
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
	})
	require.NoError(t, err)
	require.NoError(t, dc.client.StartExec(exec.ID, docker.StartExecOptions{
		OutputStream: &stdout,
		ErrorStream:  &stderr,
		RawTerminal:  false,
	}))

	inspect, err := dc.client.InspectExec(exec.ID)
	errOut := stderr.String()
	require.NoErrorf(t, err, "with stderr: %s", errOut)
	require.Zero(t, inspect.ExitCode, "with stderr: %s", errOut)
	return stdout.String()
}

// EnsureNodeReadinessByLogs checks the container's readiness by monitoring its logs and ensure its running correctly.
func (dc *DatabaseContainer) EnsureNodeReadinessByLogs(t *testing.T, requiredOutput string) {
	t.Helper()
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		output := dc.GetContainerLogs(t)
		require.Contains(ct, output, requiredOutput)
	}, 45*time.Second, 250*time.Millisecond)
}
