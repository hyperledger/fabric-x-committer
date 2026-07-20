/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"crypto/tls"
	"fmt"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/hyperledger/fabric-x-common/tools/cryptogen"
	"github.com/hyperledger/fabric-x-common/utils/connection"
	"github.com/hyperledger/fabric-x-common/utils/retry"
	"github.com/hyperledger/fabric-x-common/utils/serve"
	"github.com/hyperledger/fabric-x-common/utils/test"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

var (
	// OrgRootCA is the path to organization 0's TLS client credentials in the crypto materials directory.
	OrgRootCA = filepath.Join(cryptogen.PeerOrganizationsDir, "peer-org-0.com",
		cryptogen.MSPDir, cryptogen.TLSCaCertsDir, "tlspeer-org-0-CA-cert.pem")

	// OrdererRootCATLSPath is the path to organization 0's orderer TLS credentials in the crypto materials directory.
	OrdererRootCATLSPath = filepath.Join(cryptogen.OrdererOrganizationsDir,
		"orderer-org-0.com", cryptogen.MSPDir, cryptogen.TLSCaCertsDir, "tlsorderer-org-0-CA-cert.pem")
)

// GrpcServiceToConnectionServerConfigs extracts gRPC server endpoints from serve configs.
func GrpcServiceToConnectionServerConfigs(servers ...*serve.Config) []*serve.ServerConfig {
	result := make([]*serve.ServerConfig, len(servers))
	for i, server := range servers {
		result[i] = &server.GRPC
	}
	return result
}

// ServerToMultiClientConfig is used to create a multi client configuration from existing server(s)
// given a client TLS configuration.
func ServerToMultiClientConfig(
	clientTLS connection.TLSConfig, servers ...*serve.Config,
) *connection.MultiClientConfig {
	endpoints := make([]*connection.Endpoint, len(servers))
	for i, server := range servers {
		endpoints[i] = &server.GRPC.Endpoint
	}
	return &connection.MultiClientConfig{
		TLS:       clientTLS,
		Endpoints: endpoints,
	}
}

// NewSecuredConnection creates the default connection with given transport credentials.
func NewSecuredConnection(
	t *testing.T,
	endpoint connection.WithAddress,
	tlsConfig connection.TLSConfig,
) *grpc.ClientConn {
	t.Helper()
	return NewSecuredConnectionWithRetry(t, endpoint, tlsConfig, test.DefaultGrpcRetryProfile)
}

// NewSecuredConnectionWithRetry creates the default connection with given transport credentials.
func NewSecuredConnectionWithRetry(
	t *testing.T,
	endpoint connection.WithAddress,
	tlsConfig connection.TLSConfig,
	retryProfile retry.Profile,
) *grpc.ClientConn {
	t.Helper()
	clientCreds, err := tlsConfig.ClientCredentials()
	require.NoError(t, err)
	conn, err := connection.NewConnection(connection.ClientParameters{
		Address: endpoint.Address(),
		Creds:   clientCreds,
		Retry:   &retryProfile,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = conn.Close()
	})
	return conn
}

// NewTLSMultiClientConfig creates a multi client configuration for test purposes
// given number of endpoints and a TLS configuration.
func NewTLSMultiClientConfig(
	tlsConfig connection.TLSConfig,
	ep ...*connection.Endpoint,
) *connection.MultiClientConfig {
	return &connection.MultiClientConfig{
		Endpoints: ep,
		TLS:       tlsConfig,
		Retry:     &test.DefaultGrpcRetryProfile,
	}
}

// NewInsecureClientConfig creates a client configuration for test purposes given an endpoint.
func NewInsecureClientConfig(ep *connection.Endpoint) *connection.ClientConfig {
	return NewTLSClientConfig(test.InsecureTLSConfig, ep)
}

// NewTLSClientConfig creates a client configuration for test purposes given a single endpoint and creds.
func NewTLSClientConfig(tlsConfig connection.TLSConfig, ep *connection.Endpoint) *connection.ClientConfig {
	return &connection.ClientConfig{
		Endpoint: ep,
		TLS:      tlsConfig,
		Retry:    &test.DefaultGrpcRetryProfile,
	}
}

// MustGetTLSConfig creates a tls.Config from a connection.TLSConfig while ensuring no error return from that process.
func MustGetTLSConfig(t *testing.T, tlsConfig *connection.TLSConfig) *tls.Config {
	t.Helper()
	if tlsConfig == nil {
		return nil
	}
	tlsCreds, err := connection.NewClientTLSCredentials(*tlsConfig)
	require.NoError(t, err)
	clientTLSConfig, err := tlsCreds.CreateClientTLSConfig()
	require.NoError(t, err)
	return clientTLSConfig
}

// NewPreAllocatedLocalHostServerConfig create a localhost server config with a pre allocated listener and port.
func NewPreAllocatedLocalHostServerConfig(t *testing.T, tlsConfig connection.TLSConfig) *serve.Config {
	t.Helper()
	serverConfig := test.NewLocalHostServiceConfig(tlsConfig)
	serve.PreAllocateListener(t, &serverConfig.GRPC)
	return serverConfig
}

// NewServiceTLSConfig creates a server TLS configuration with certificates loaded from the artifact path.
// This function constructs paths to TLS certificates for a given service within the peer organization structure.
func NewServiceTLSConfig(artifactsPath, serviceName, mode string) connection.TLSConfig {
	subPath := filepath.Join(artifactsPath, cryptogen.PeerOrganizationsDir, "peer-org-0.com",
		cryptogen.PeerNodesDir, serviceName, cryptogen.TLSDir)
	return connection.TLSConfig{
		Mode:     mode,
		CertPath: filepath.Join(subPath, "server.crt"),
		KeyPath:  filepath.Join(subPath, "server.key"),
		CACertPaths: []string{
			filepath.Join(artifactsPath, OrgRootCA),
		},
	}
}

// NewEndpoint creates an endpoint from give host and port (as string).
func NewEndpoint(t *testing.T, host, port string) *connection.Endpoint {
	t.Helper()
	convertedPort, err := strconv.Atoi(port)
	require.NoError(t, err, "could not convert port to integer")
	return &connection.Endpoint{Host: host, Port: convertedPort}
}

// OrgClientTLSConfig creates a mutual TLS client configuration using a specific
// peer organization's TLS client certificate. The serverCACertPaths are the CA
// certs needed to verify the server (typically from the CredentialsFactory).
func OrgClientTLSConfig(artifactsPath string, orgIndex int, serverCACertPaths []string) connection.TLSConfig {
	orgDomain := fmt.Sprintf("peer-org-%d.com", orgIndex)
	tlsDir := filepath.Join(artifactsPath, cryptogen.PeerOrganizationsDir, orgDomain,
		cryptogen.UsersDir, fmt.Sprintf("client@%s", orgDomain), cryptogen.TLSDir)
	return connection.TLSConfig{
		Mode:        connection.MutualTLSMode,
		CertPath:    filepath.Join(tlsDir, "client.crt"),
		KeyPath:     filepath.Join(tlsDir, "client.key"),
		CACertPaths: serverCACertPaths,
	}
}
