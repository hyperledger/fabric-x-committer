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
	"github.com/hyperledger/fabric-x-committer/utils/tlsgen"
)

type (
	// SecureConnectionFunctionArguments groups the arguments required to run a secure connection test.
	SecureConnectionFunctionArguments struct {
		ServerCN      string
		ServerStarter ServerStarter
		ClientStarter ClientStarter
		Parallel      bool
	}

	// ServerStarter is a func that receives a TLS config, start the server and return its endpoint.
	ServerStarter func(t *testing.T, serverTLS *connection.TLSConfig) (endpoint connection.Endpoint)

	// ClientStarter dials the service and returns a func that executes a request.
	ClientStarter func(t *testing.T, endpoint *connection.Endpoint, cfg *connection.TLSConfig) RequestFunc

	// RequestFunc performs a request and returns the resulting error.
	RequestFunc func(ctx context.Context) error
)

// RunSecureConnectionTest starts a gRPC server with mTLS enabled and
// tests client connections using various TLS configurations to verify that
// the server correctly accepts or rejects connections based on the client's setup.
func RunSecureConnectionTest(
	t *testing.T,
	secureConnArguments SecureConnectionFunctionArguments,
) {
	t.Helper()
	tlsMgr := tlsgen.NewSecureCommunicationManager(t)
	serverCreds := tlsMgr.CreateServerCertificate(t, secureConnArguments.ServerCN)
	serverTLS := CreateTLSConfigFromPaths(connection.MutualTLSMode, serverCreds, "")
	endpoint := secureConnArguments.ServerStarter(t, &serverTLS)

	clientCreds := tlsMgr.CreateClientCertificate(t)
	baseClientTLS := CreateTLSConfigFromPaths(connection.NoneTLSMode, clientCreds, secureConnArguments.ServerCN)

	cases := []struct {
		desc      string
		tlsMode   string
		expectErr bool
	}{
		{"correct client credentials", connection.MutualTLSMode, false},
		{"without mTLS", connection.ServerSideTLSMode, true},
		{"with no TLS at all", connection.NoneTLSMode, true},
	}

	for _, tc := range cases {
		testCase := tc
		t.Run(fmt.Sprintf("%s/%s", secureConnArguments.ServerCN, testCase.desc), func(t *testing.T) {
			if secureConnArguments.Parallel {
				t.Parallel()
			}

			cfg := baseClientTLS
			cfg.Mode = testCase.tlsMode

			requestFunc := secureConnArguments.ClientStarter(t, &endpoint, &cfg)

			ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
			t.Cleanup(cancel)

			err := requestFunc(ctx)
			if tc.expectErr {
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

	creds, err := tlsCfg.ClientOption()
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
	serverName string,
) connection.TLSConfig {
	return connection.TLSConfig{
		Mode:        connectionMode,
		ServerName:  serverName,
		KeyPath:     paths["private-key"],
		CertPath:    paths["public-key"],
		CACertPaths: []string{paths["ca-certificate"]},
	}
}
