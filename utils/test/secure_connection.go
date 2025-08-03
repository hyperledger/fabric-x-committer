/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

type (
	// SecureConnectionArguments groups the arguments required to run a secure connection test.
	SecureConnectionArguments struct {
		Service       string
		ServerTLSMode string
		TestCases     []Case
		ServerStarter ServerStarter
		ClientStarter ClientStarter
		Parallel      bool
	}

	// Case define a secure connection test case.
	Case struct {
		testDescription  string
		clientSecureMode string
		shouldFail       bool
	}

	// ServerStarter is a func that receives a TLS config, start the server and return its endpoint.
	ServerStarter func(t *testing.T, serverTLS *connection.TLSConfig) (endpoint connection.Endpoint)

	// ClientStarter dials the service and returns a func that executes a request.
	ClientStarter func(t *testing.T, endpoint *connection.Endpoint, cfg *connection.TLSConfig) RequestFunc

	// RequestFunc performs a request and returns the resulting error.
	RequestFunc func(ctx context.Context) error
)

const defaultHostName = "localhost"

// ServerModes is a list of server-side TLS modes used for testing.
var ServerModes = []string{connection.MutualTLSMode, connection.ServerSideTLSMode, connection.NoneTLSMode}

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
	secureConnArguments SecureConnectionArguments,
) {
	t.Helper()
	tlsMgr := NewSecureCommunicationManager(t)
	serverCreds := tlsMgr.CreateServerCertificate(t, defaultHostName)
	serverTLS := CreateTLSConfigFromPaths(secureConnArguments.ServerTLSMode, serverCreds)
	endpoint := secureConnArguments.ServerStarter(t, &serverTLS)

	clientCreds := tlsMgr.CreateClientCertificate(t)
	baseClientTLS := CreateTLSConfigFromPaths(connection.NoneTLSMode, clientCreds)

	for _, tc := range secureConnArguments.TestCases {
		testCase := tc
		t.Run(fmt.Sprintf(
			"%s/%s/%s",
			secureConnArguments.Service,
			secureConnArguments.ServerTLSMode,
			testCase.testDescription,
		), func(t *testing.T) {
			if secureConnArguments.Parallel {
				t.Parallel()
			}

			cfg := baseClientTLS
			cfg.Mode = testCase.clientSecureMode

			requestFunc := secureConnArguments.ClientStarter(t, &endpoint, &cfg)

			ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
			t.Cleanup(cancel)

			err := requestFunc(ctx)
			if tc.shouldFail {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
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
	tlsCfg *connection.TLSConfig,
	protoClient func(grpc.ClientConnInterface) T,
) T {
	t.Helper()

	creds, err := tlsCfg.ClientCredentials()
	require.NoError(t, err)

	conn, err := connection.Connect(
		connection.NewDialConfigWithCreds(endpoint, creds),
	)
	require.NoError(t, err)

	t.Cleanup(func() { require.NoError(t, conn.Close()) })

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
