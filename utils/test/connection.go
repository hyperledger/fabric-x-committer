/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"context"
	"crypto/tls"
	"fmt"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/hyperledger/fabric-x-common/tools/cryptogen"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/retry"
)

var (
	// InsecureTLSConfig defines an empty tls config.
	InsecureTLSConfig connection.TLSConfig
	// defaultGrpcRetryProfile defines the retry policy for a gRPC client connection.
	defaultGrpcRetryProfile retry.Profile

	// OrgRootCA is the path to organization 0's TLS client credentials in the crypto materials directory.
	OrgRootCA = filepath.Join(cryptogen.PeerOrganizationsDir, "peer-org-0",
		cryptogen.MSPDir, cryptogen.TLSCaCertsDir, "tlspeer-org-0-CA-cert.pem")

	// OrdererRootCATLSPath is the path to organization 0's orderer TLS credentials in the crypto materials directory.
	OrdererRootCATLSPath = filepath.Join(cryptogen.OrdererOrganizationsDir,
		"orderer-org-0", cryptogen.MSPDir, cryptogen.TLSCaCertsDir, "tlsorderer-org-0-CA-cert.pem")
)

// CheckServerStopped returns true if the grpc server listening on a
// given address has been stopped.
func CheckServerStopped(t *testing.T, addr string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	conn, err := grpc.DialContext( //nolint:staticcheck
		ctx,
		addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(), //nolint:staticcheck
	)
	if err != nil {
		return true
	}
	_ = conn.Close()
	return false
}

// ServerToMultiClientConfig is used to create a multi client configuration from existing server(s)
// given a client TLS configuration.
func ServerToMultiClientConfig(
	clientTLS connection.TLSConfig, servers ...*connection.ServerConfig,
) *connection.MultiClientConfig {
	endpoints := make([]*connection.Endpoint, len(servers))
	for i, server := range servers {
		endpoints[i] = &server.Endpoint
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
	return NewSecuredConnectionWithRetry(t, endpoint, tlsConfig, defaultGrpcRetryProfile)
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

// NewInsecureConnection creates the default connection with insecure credentials.
func NewInsecureConnection(t *testing.T, endpoint connection.WithAddress) *grpc.ClientConn {
	t.Helper()
	return NewInsecureConnectionWithRetry(t, endpoint, defaultGrpcRetryProfile)
}

// NewInsecureConnectionWithRetry creates the default dial config with insecure credentials.
func NewInsecureConnectionWithRetry(
	t *testing.T, endpoint connection.WithAddress, retryProfile retry.Profile,
) *grpc.ClientConn {
	t.Helper()
	conn, err := connection.NewConnection(connection.ClientParameters{
		Address: endpoint.Address(),
		Creds:   insecure.NewCredentials(),
		Retry:   &retryProfile,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = conn.Close()
	})
	return conn
}

// NewInsecureLoadBalancedConnection creates the default connection with insecure credentials.
func NewInsecureLoadBalancedConnection(t *testing.T, endpoints []*connection.Endpoint) *grpc.ClientConn {
	t.Helper()
	conn, err := connection.NewLoadBalancedConnection(&connection.MultiClientConfig{
		Endpoints: endpoints,
		Retry:     &defaultGrpcRetryProfile,
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
		Retry:     &defaultGrpcRetryProfile,
	}
}

// NewInsecureClientConfig creates a client configuration for test purposes given an endpoint.
func NewInsecureClientConfig(ep *connection.Endpoint) *connection.ClientConfig {
	return NewTLSClientConfig(InsecureTLSConfig, ep)
}

// NewTLSClientConfig creates a client configuration for test purposes given a single endpoint and creds.
func NewTLSClientConfig(tlsConfig connection.TLSConfig, ep *connection.Endpoint) *connection.ClientConfig {
	return &connection.ClientConfig{
		Endpoint: ep,
		TLS:      tlsConfig,
		Retry:    &defaultGrpcRetryProfile,
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

// NewPreAllocatedLocalHostServer create a localhost server config with a pre allocated listener and port.
func NewPreAllocatedLocalHostServer(t *testing.T, tlsConfig connection.TLSConfig) *connection.ServerConfig {
	t.Helper()
	server := NewLocalHostServer(tlsConfig)
	listener, err := server.PreAllocateListener(t.Context())
	t.Cleanup(func() {
		_ = listener.Close()
	})
	require.NoError(t, err)
	return server
}

// NewServiceTLSConfig creates a server TLS configuration with certificates loaded from the artifact path.
// This function constructs paths to TLS certificates for a given service within the peer organization structure.
func NewServiceTLSConfig(artifactsPath, serviceName, mode string) connection.TLSConfig {
	subPath := filepath.Join(artifactsPath, cryptogen.PeerOrganizationsDir, "peer-org-0",
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

// NewLocalHostServer returns a default server config with endpoint "localhost:0" given server credentials.
func NewLocalHostServer(creds connection.TLSConfig) *connection.ServerConfig {
	return &connection.ServerConfig{
		Endpoint: connection.Endpoint{Host: "127.0.0.1"},
		TLS:      creds,
	}
}

// OrgClientTLSConfig creates a mutual TLS client configuration using a specific
// peer organization's TLS client certificate. The serverCACertPaths are the CA
// certs needed to verify the server (typically from the CredentialsFactory).
func OrgClientTLSConfig(artifactsPath string, orgIndex int, serverCACertPaths []string) connection.TLSConfig {
	orgName := fmt.Sprintf("peer-org-%d", orgIndex)
	orgDomain := fmt.Sprintf("peer-org-%d.com", orgIndex)
	tlsDir := filepath.Join(artifactsPath, cryptogen.PeerOrganizationsDir, orgName,
		cryptogen.UsersDir, fmt.Sprintf("client@%s", orgDomain), cryptogen.TLSDir)
	return connection.TLSConfig{
		Mode:        connection.MutualTLSMode,
		CertPath:    filepath.Join(tlsDir, "client.crt"),
		KeyPath:     filepath.Join(tlsDir, "client.key"),
		CACertPaths: serverCACertPaths,
	}
}
