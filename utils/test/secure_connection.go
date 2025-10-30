/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hyperledger/fabric-x-common/common/crypto/tlsgen"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

type (
	// CredentialsFactory responsible for the creation of
	// TLS certificates inside a TLSConfig for testing purposes
	// by using the tls generation library of 'Hyperledger Fabric'.
	CredentialsFactory struct {
		CertificateAuthority tlsgen.CA
	}

	// testCase define a secure connection test case.
	testCase struct {
		testDescription  string
		clientSecureMode string
		shouldFail       bool
	}

	createTLSConfigParameters struct {
		connectionMode string
		keyPair        *tlsgen.CertKeyPair
		namingStyle    string
	}

	// ServerStarter is a function that receives a TLS configuration, starts the server,
	// and returns a RPCAttempt function for initiating a client connection and attempting an RPC call.
	ServerStarter func(t *testing.T, serverTLS connection.TLSConfig) RPCAttempt

	// RPCAttempt is a function returned by ServerStarter that contains the information
	// needed to start a client connection and attempt an RPC call.
	RPCAttempt func(ctx context.Context, t *testing.T, cfg connection.TLSConfig) error
)

const (
	defaultHostName = "localhost"

	// YugaDBType represents the usage of Yugabyte DB.
	YugaDBType = "yugabyte"
	// PostgresDBType represents the usage of PostgreSQL DB.
	PostgresDBType = "postgres"
	// DefaultCertStyle represents the default TLS certificate style creation.
	DefaultCertStyle = "default"
	//nolint:revive // KeyPrivate, KeyPublic and KeyCACert represents the chosen key in the naming function.
	KeyPrivate = "private-key"
	KeyPublic  = "public-key"
	KeyCACert  = "ca-certificate"
)

// ServerModes is a list of server-side TLS modes used for testing.
var ServerModes = []string{connection.MutualTLSMode, connection.OneSideTLSMode, connection.NoneTLSMode}

// NewCredentialsFactory returns a CredentialsFactory with a new CA.
func NewCredentialsFactory(t *testing.T) *CredentialsFactory {
	t.Helper()
	ca, err := tlsgen.NewCA()
	require.NoError(t, err)
	return &CredentialsFactory{
		CertificateAuthority: ca,
	}
}

// CreateServerCredentials creates a server key pair given SAN(s) (Subject Alternative Name),
// Writing it to a temp testing folder and returns a [connection.TLSConfig].
func (scm *CredentialsFactory) CreateServerCredentials(
	t *testing.T,
	tlsMode string,
	namingStyle string,
	san ...string,
) (connection.TLSConfig, string) {
	t.Helper()
	serverKeypair, err := scm.CertificateAuthority.NewServerCertKeyPair(san...)
	require.NoError(t, err)
	return scm.createTLSConfig(t, createTLSConfigParameters{
		connectionMode: tlsMode,
		keyPair:        serverKeypair,
		namingStyle:    namingStyle,
	})
}

// CreateClientCredentials creates a client key pair,
// Writing it to a temp testing folder and returns a [connection.TLSConfig].
func (scm *CredentialsFactory) CreateClientCredentials(t *testing.T, tlsMode string) (connection.TLSConfig, string) {
	t.Helper()
	clientKeypair, err := scm.CertificateAuthority.NewClientCertKeyPair()
	require.NoError(t, err)
	return scm.createTLSConfig(t, createTLSConfigParameters{
		connectionMode: tlsMode,
		keyPair:        clientKeypair,
		namingStyle:    DefaultCertStyle,
	})
}

/*
RunSecureConnectionTest starts a gRPC server with mTLS enabled and
tests client connections using various TLS configurations to verify that
the server correctly accepts or rejects connections based on the client's setup.
It runs a server instance of the service and returns a function
that starts a client with the required TLS mode, attempts an RPC call,
and returns the resulting error.
Server Mode | Client with mTLS | Client with server-side TLS | Client with no TLS
------------|------------------|-----------------------------|--------------------
mTLS        |      connect     |        can't connect        |     can't connect
TLS         |      connect     |           connect           |     can't connect
None        | can't connect    |        can't connect        |       connect.
*/
func RunSecureConnectionTest(
	t *testing.T,
	starter ServerStarter,
) {
	t.Helper()
	// create server and client credentials
	tlsMgr := NewCredentialsFactory(t)
	// create a base TLS configuration for the client
	baseClientTLS, _ := tlsMgr.CreateClientCredentials(t, connection.NoneTLSMode)
	for _, tc := range []struct {
		serverMode string
		cases      []testCase
	}{
		{
			serverMode: connection.MutualTLSMode,
			cases: []testCase{
				{"client mTLS", connection.MutualTLSMode, false},
				{"client with one sided TLS", connection.OneSideTLSMode, true},
				{"client no TLS", connection.NoneTLSMode, true},
			},
		},
		{
			serverMode: connection.OneSideTLSMode,
			cases: []testCase{
				{"client mTLS", connection.MutualTLSMode, false},
				{"client with one sided TLS", connection.OneSideTLSMode, false},
				{"client no TLS", connection.NoneTLSMode, true},
			},
		},
		{
			serverMode: connection.NoneTLSMode,
			cases: []testCase{
				{"client mTLS", connection.MutualTLSMode, true},
				{"client with one sided TLS", connection.OneSideTLSMode, true},
				{"client no TLS", connection.NoneTLSMode, false},
			},
		},
	} {
		t.Run(fmt.Sprintf("server-tls:%s", tc.serverMode), func(t *testing.T) {
			t.Parallel()
			// create server's tls config and start it according to the server tls mode.
			serverTLS, _ := tlsMgr.CreateServerCredentials(t, tc.serverMode, DefaultCertStyle, defaultHostName)
			rpcAttemptFunc := starter(t, serverTLS)
			// for each server secure mode, build the client's test cases.
			for _, clientTestCase := range tc.cases {
				t.Run(clientTestCase.testDescription, func(t *testing.T) {
					t.Parallel()

					cfg := baseClientTLS
					cfg.Mode = clientTestCase.clientSecureMode

					ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
					t.Cleanup(cancel)

					err := rpcAttemptFunc(ctx, t, cfg)
					if clientTestCase.shouldFail {
						require.Error(t, err)
					} else {
						require.NoError(t, err)
					}
				})
			}
		})
	}
}

// CreateClientWithTLS creates and returns a typed gRPC client using the provided TLS configuration.
// It establishes a secure connection to the given endpoint
// and returns the generated client using the provided client creation proto function.
func CreateClientWithTLS[T any](
	t *testing.T,
	endpoint *connection.Endpoint,
	tlsCfg connection.TLSConfig,
	protoClient func(grpc.ClientConnInterface) T,
) T {
	t.Helper()
	dialConfig := NewSecuredDialConfig(t, endpoint, tlsCfg)
	// prevents secure connection tests from hanging until the context times out.
	dialConfig.SetRetryProfile(&connection.RetryProfile{
		MaxElapsedTime: 3 * time.Second,
	})
	conn, err := connection.Connect(dialConfig)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, conn.Close())
	})
	return protoClient(conn)
}

// createTLSConfig creates a TLS configuration based on the
// given TLS mode and credential bytes, and returns it along with the certificates' path.
func (scm *CredentialsFactory) createTLSConfig(
	t *testing.T,
	params createTLSConfigParameters,
) (connection.TLSConfig, string) {
	t.Helper()
	tmpDir := t.TempDir()
	namingFunction := selectFileNames(params.namingStyle)

	privateKeyPath := filepath.Join(tmpDir, namingFunction("private-key"))
	require.NoError(t, os.WriteFile(privateKeyPath, params.keyPair.Key, 0o600))

	publicKeyPath := filepath.Join(tmpDir, namingFunction("public-key"))
	require.NoError(t, os.WriteFile(publicKeyPath, params.keyPair.Cert, 0o600))

	caCertificatePath := filepath.Join(tmpDir, namingFunction("ca-certificate"))
	require.NoError(t, os.WriteFile(caCertificatePath, scm.CertificateAuthority.CertBytes(), 0o600))

	return connection.TLSConfig{
		Mode:        params.connectionMode,
		KeyPath:     privateKeyPath,
		CertPath:    publicKeyPath,
		CACertPaths: []string{caCertificatePath},
	}, tmpDir
}

func selectFileNames(style string) func(string) string {
	switch style {
	case YugaDBType:
		return func(key string) string {
			switch key {
			// We currently use YugabyteDB with the hostname "db" only.
			// To support additional instances with different hostnames,
			// replace "db" with the desired hostname when creating the instance.
			case KeyPublic:
				return "node.db.crt"
			case KeyPrivate:
				return "node.db.key"
			case KeyCACert:
				return "ca.crt"
			default:
				return ""
			}
		}
	case PostgresDBType:
		return func(key string) string {
			switch key {
			case KeyPublic:
				return "server.crt"
			case KeyPrivate:
				return "server.key"
			case KeyCACert:
				return "ca-certificate.crt"
			default:
				return ""
			}
		}
	default:
		return func(key string) string {
			switch key {
			case KeyPublic:
				return "public-key.crt"
			case KeyPrivate:
				return "private-key.key"
			case KeyCACert:
				return "ca-certificate.crt"
			default:
				return ""
			}
		}
	}
}
