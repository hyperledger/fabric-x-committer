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

	"github.com/hyperledger/fabric/common/crypto/tlsgen"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

type (
	// CertificateManager responsible for the creation of
	// TLS certificates for testing purposes by utilizing the tls generation library of 'Hyperledger Fabric'.
	// Path map convention: private-key, public-key, ca-certificate.
	CertificateManager struct {
		CertificateAuthority tlsgen.CA
	}

	// SecureConnectionParameters groups the parameters required to run a secure connection test.
	SecureConnectionParameters struct {
		Service       string
		ServerTLSMode string
		TestCases     []Case
		ServerStarter ServerStarter
	}

	// Case define a secure connection test case.
	Case struct {
		testDescription  string
		clientSecureMode string
		shouldFail       bool
	}

	// ServerStarter is a function that receives a TLS configuration, starts the server,
	// and returns a ClientStarter function for initiating a client connection.
	ServerStarter func(t *testing.T, serverTLS *connection.TLSConfig) ClientStarter

	// ClientStarter is a function returned by ServerStarter that contains the information
	// needed to start a client connection.
	ClientStarter func(ctx context.Context, t *testing.T, cfg *connection.TLSConfig) error
)

const defaultHostName = "localhost"

// ServerModes is a list of server-side TLS modes used for testing.
var ServerModes = []string{connection.MutualTLSMode, connection.ServerSideTLSMode, connection.NoneTLSMode}

// NewTLSCertificateManager returns a CertificateManager with a new CA.
func NewTLSCertificateManager(t *testing.T) *CertificateManager {
	t.Helper()
	ca, err := tlsgen.NewCA()
	require.NoError(t, err)
	return &CertificateManager{
		CertificateAuthority: ca,
	}
}

// CreateServerCertificate creates a server key pair given SAN (Subject Alternative Name),
// Writing it to a temp testing folder and returns a map with the credential paths.
func (scm *CertificateManager) CreateServerCertificate(
	t *testing.T,
	san string,
) map[string]string {
	t.Helper()
	serverKeypair, err := scm.CertificateAuthority.NewServerCertKeyPair(san)
	require.NoError(t, err)
	return createCertificatesPaths(t, createDataFromKeyPair(serverKeypair, scm.CertificateAuthority.CertBytes()))
}

// CreateClientCertificate creates a client key pair,
// Writing it to a temp testing folder and returns a map with the credential paths.
func (scm *CertificateManager) CreateClientCertificate(t *testing.T) map[string]string {
	t.Helper()
	clientKeypair, err := scm.CertificateAuthority.NewClientCertKeyPair()
	require.NoError(t, err)
	return createCertificatesPaths(t, createDataFromKeyPair(clientKeypair, scm.CertificateAuthority.CertBytes()))
}

func createCertificatesPaths(t *testing.T, data map[string][]byte) map[string]string {
	t.Helper()
	tmpDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, os.RemoveAll(tmpDir))
	})

	paths := make(map[string]string)

	for key, value := range data {
		dataPath, err := saveBytesToFile(tmpDir, key, value)
		require.NoError(t, err)
		paths[key] = dataPath
	}
	return paths
}

func createDataFromKeyPair(keyPair *tlsgen.CertKeyPair, caCertificate []byte) map[string][]byte {
	data := make(map[string][]byte)
	data["private-key"] = keyPair.Key
	data["public-key"] = keyPair.Cert
	data["ca-certificate"] = caCertificate
	return data
}

func saveBytesToFile(dir, filename string, data []byte) (string, error) {
	filePath := filepath.Join(dir, filename)
	return filePath, os.WriteFile(filePath, data, 0o600)
}

// BuildTestCases creates test cases based on the server's TLS mode.
//
// The cases and their expected results:
// Server Mode | Client with mTLS | Client with server-side TLS | Client with no TLS
// ------------|------------------|-----------------------------|--------------------
//
//	mTLS    |      connect     |        can't connect        |     can't connect
//	TLS     |      connect     |           connect           |     can't connect
//	None    |   can't connect  |        can't connect        |       connect
func BuildTestCases(t *testing.T, serverTLSMode string) []Case {
	t.Helper()

	switch serverTLSMode {
	case connection.MutualTLSMode:
		return []Case{
			{"client mTLS", connection.MutualTLSMode, false},
			{"client with server-side TLS", connection.ServerSideTLSMode, true},
			{"client no TLS", connection.NoneTLSMode, true},
		}
	case connection.ServerSideTLSMode:
		return []Case{
			{"client mTLS", connection.MutualTLSMode, false},
			{"client with server-side TLS", connection.ServerSideTLSMode, false},
			{"client no TLS", connection.NoneTLSMode, true},
		}
	case connection.NoneTLSMode:
		return []Case{
			{"client mTLS", connection.MutualTLSMode, true},
			{"client with server-side TLS", connection.ServerSideTLSMode, true},
			{"client no TLS", connection.NoneTLSMode, false},
		}
	default:
		t.Fatalf("unknown server TLS mode: %s", serverTLSMode)
		return nil // unreachable, but required
	}
}

// RunSecureConnectionTest starts a gRPC server with mTLS enabled and
// tests client connections using various TLS configurations to verify that
// the server correctly accepts or rejects connections based on the client's setup.
func RunSecureConnectionTest(
	t *testing.T,
	secureConnParameters SecureConnectionParameters,
) {
	t.Helper()
	// create server and client credentials
	tlsMgr := NewTLSCertificateManager(t)
	serverCreds := tlsMgr.CreateServerCertificate(t, defaultHostName)
	clientCreds := tlsMgr.CreateClientCertificate(t)
	// create a base TLS configuration for the client
	baseClientTLS := CreateTLSConfigFromPaths(connection.NoneTLSMode, clientCreds)

	for _, serverSecureMode := range ServerModes {
		// create server's tls config and start it according to the serverSecureMode.
		serverTLS := CreateTLSConfigFromPaths(serverSecureMode, serverCreds)
		clientStarterFunc := secureConnParameters.ServerStarter(t, &serverTLS)
		// for each server secure mode, build the client's test cases.
		for _, tc := range BuildTestCases(t, serverSecureMode) {
			testCase := tc
			t.Run(fmt.Sprintf(
				"%s/%s/%s",
				secureConnParameters.Service,
				serverSecureMode,
				testCase.testDescription,
			), func(t *testing.T) {
				t.Parallel()

				cfg := baseClientTLS
				cfg.Mode = testCase.clientSecureMode

				ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
				t.Cleanup(cancel)

				err := clientStarterFunc(ctx, t, &cfg)
				if tc.shouldFail {
					require.Error(t, err)
				} else {
					require.NoError(t, err)
				}
			})
		}
	}
}

// CreateClientWithTLS creates and returns a typed gRPC client using the provided TLS configuration.
// It establishes a secure connection to the given endpoint
// and returns the generated client using the provided client creation proto function.
func CreateClientWithTLS[T any](
	t *testing.T,
	endpoint *connection.Endpoint,
	tlsCfg *connection.TLSConfig,
	protoClient func(grpc.ClientConnInterface) T,
) T {
	t.Helper()
	conn, err := connection.Connect(NewSecuredDialConfig(t, endpoint, tlsCfg))
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, conn.Close())
	})
	return protoClient(conn)
}

// CreateTLSConfigFromPaths creates and returns a TLS configuration based on the
// given TLS mode, credential file paths, and the expected server name.
func CreateTLSConfigFromPaths(
	connectionMode string,
	paths map[string]string,
) connection.TLSConfig {
	return connection.TLSConfig{
		Mode:        connectionMode,
		KeyPath:     paths["private-key"],
		CertPath:    paths["public-key"],
		CACertPaths: []string{paths["ca-certificate"]},
	}
}
