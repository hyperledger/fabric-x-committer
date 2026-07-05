/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/Shopify/toxiproxy/v2"
	toxiclient "github.com/Shopify/toxiproxy/v2/client"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/hyperledger/fabric-x-committer/integration/runner"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

const (
	keepAliveTime    = 5 * time.Second
	keepAliveTimeout = 10 * time.Second

	// The server should close the connection within Time and Timeout,
	// but we add some amount of time so the context will not finish before.
	connectionClosingTime = keepAliveTime + keepAliveTimeout + 2*time.Minute

	localhostDynamicPort = "localhost:0"
	dummyTxID            = "dummy-tx"
)

func TestKeepAliveDeadConnectionDetection(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name                 string
		permitWithoutStream  bool
		maxConcurrentStreams int
		serviceAddr          func(*runner.CommitterRuntime) string
		connect              func(
			t *testing.T,
			proxiedConn *grpc.ClientConn,
			directAddr string,
			clientCreds credentials.TransportCredentials,
		) func(*testing.T)
	}{
		{
			name:                "Sidecar",
			permitWithoutStream: false,
			serviceAddr: func(c *runner.CommitterRuntime) string {
				return c.SystemConfig.Services.Sidecar.GrpcEndpoint.Address()
			},
			connect: func(
				t *testing.T,
				proxiedConn *grpc.ClientConn,
				_ string,
				_ credentials.TransportCredentials,
			) func(*testing.T) {
				t.Helper()
				client := committerpb.NewNotifierClient(proxiedConn)
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
				t.Cleanup(cancel)

				stream, err := client.OpenNotificationStream(ctx)
				require.NoError(t, err)
				require.NoError(t, stream.Send(&committerpb.NotificationRequest{
					TxStatusRequest: &committerpb.TxIDsBatch{TxIds: []string{dummyTxID}},
				}))

				return func(t *testing.T) {
					t.Helper()
					receiveErr := receiveWithin(t, stream, connectionClosingTime)
					require.Error(t, receiveErr, "server should close the dead connection via keep-alive")
					require.Equal(
						t,
						codes.Unavailable, status.Code(receiveErr), "expected server-initiated close",
					)
				}
			},
		},
		{
			name:                 "SidecarStreamSlotRelease",
			permitWithoutStream:  false,
			maxConcurrentStreams: 4,
			serviceAddr: func(c *runner.CommitterRuntime) string {
				return c.SystemConfig.Services.Sidecar.GrpcEndpoint.Address()
			},
			connect: func(
				t *testing.T,
				proxiedConn *grpc.ClientConn,
				directAddr string,
				clientCreds credentials.TransportCredentials,
			) func(*testing.T) {
				t.Helper()
				client1 := committerpb.NewNotifierClient(proxiedConn)

				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
				t.Cleanup(cancel)

				stream1, err := client1.OpenNotificationStream(ctx)
				require.NoError(t, err)
				require.NoError(t, stream1.Send(&committerpb.NotificationRequest{
					TxStatusRequest: &committerpb.TxIDsBatch{TxIds: []string{dummyTxID}},
				}))

				// Create direct connection
				conn2, err := grpc.NewClient(directAddr, grpc.WithTransportCredentials(clientCreds))
				require.NoError(t, err)
				t.Cleanup(func() { _ = conn2.Close() })

				client2 := committerpb.NewNotifierClient(conn2)
				// Verify slot is occupied
				stream2, err := client2.OpenNotificationStream(ctx)
				if err == nil {
					_, err = stream2.Recv()
				}
				require.Error(t, err, "second stream should fail with ResourceExhausted")

				return func(t *testing.T) {
					t.Helper()
					ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
					t.Cleanup(cancel)

					// Verify slot is released after keep-alive timeout
					require.EventuallyWithT(t, func(ct *assert.CollectT) {
						stream3, err := client2.OpenNotificationStream(ctx)
						require.NoError(ct, err, "third stream should succeed after first connection closed")
						require.NoError(ct, stream3.Send(&committerpb.NotificationRequest{
							TxStatusRequest: &committerpb.TxIDsBatch{TxIds: []string{dummyTxID}},
						}))
					}, connectionClosingTime, 500*time.Millisecond)
				}
			},
		},
		{
			name:                "Query",
			permitWithoutStream: true,
			serviceAddr: func(c *runner.CommitterRuntime) string {
				return c.SystemConfig.Services.Query.GrpcEndpoint.Address()
			},
			connect: func(
				t *testing.T,
				proxiedConn *grpc.ClientConn,
				_ string,
				_ credentials.TransportCredentials,
			) func(*testing.T) {
				t.Helper()
				client := committerpb.NewQueryServiceClient(proxiedConn)

				ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
				t.Cleanup(cancel)

				_, err := client.GetTransactionStatus(ctx, &committerpb.TxStatusQuery{
					TxIds: []string{dummyTxID},
				})
				require.NoError(t, err)

				return func(t *testing.T) {
					t.Helper()
					require.EventuallyWithT(t, func(ct *assert.CollectT) {
						require.NotEqual(ct, connectivity.Ready, proxiedConn.GetState())
					}, connectionClosingTime, 200*time.Millisecond)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			c := runner.NewRuntime(t, &runner.Config{
				BlockTimeout:                 2 * time.Second,
				KeepAliveTime:                keepAliveTime,
				KeepAliveTimeout:             keepAliveTimeout,
				KeepAlivePermitWithoutStream: tc.permitWithoutStream,
				MaxConcurrentStreams:         tc.maxConcurrentStreams,
			})
			c.Start(t, runner.FullTxPathWithQuery)

			clientCreds, err := c.SystemConfig.ClientTLS.ClientCredentials()
			require.NoError(t, err)

			serviceAddr := tc.serviceAddr(c)
			proxy := newProxy(t, serviceAddr)

			proxiedConn, err := grpc.NewClient(proxy.Listen, grpc.WithTransportCredentials(clientCreds))
			require.NoError(t, err)
			t.Cleanup(func() { _ = proxiedConn.Close() })

			proxiedConn.Connect()
			require.EventuallyWithT(t, func(ct *assert.CollectT) {
				require.Equal(ct, connectivity.Ready, proxiedConn.GetState())
			}, 30*time.Second, 500*time.Millisecond)

			verify := tc.connect(t, proxiedConn, serviceAddr, clientCreds)

			blackHole(t, proxy)

			verify(t)
		})
	}
}

// newProxy creates a proxy control plane between the client and the service that can silently
// drop all traffic on a connection without closing it.
//
// A live gRPC client cannot be made unresponsive through configuration: its
// transport automatically responds to server pings. To produce a genuinely
// silent client, the connection is routed through a proxy that blocks data transportation.
// The socket remains open, but no bytes flow, so the server's ping
// is never acknowledged and the server must close the connection itself.
func newProxy(t *testing.T, upstream string) *toxiclient.Proxy {
	t.Helper()

	// We need to pre-allocate a port for the toxiproxy control API because the library
	// doesn't expose the OS-assigned port when using "localhost:0".
	// Since the client must know the exact address to connect, we use freePort to get an available port.
	controlAddress := net.JoinHostPort("localhost", strconv.Itoa(freePort(t)))
	server := toxiproxy.NewServer(toxiproxy.NewMetricsContainer(prometheus.NewRegistry()), zerolog.Nop())

	var wg errgroup.Group
	wg.Go(func() error { return server.Listen(controlAddress) })
	t.Cleanup(func() { require.NoError(t, wg.Wait()) })
	t.Cleanup(func() { require.NoError(t, server.Shutdown()) })

	client := toxiclient.NewClient(controlAddress)
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		_, err := client.Proxies()
		require.NoError(ct, err)
	}, 15*time.Second, 50*time.Millisecond, "proxy control plane did not start")

	// the resolved address is returned in proxy.Listen.
	proxy, err := client.CreateProxy("keepalive", localhostDynamicPort, upstream)
	require.NoError(t, err)
	t.Cleanup(func() { _ = proxy.Delete() })

	return proxy
}

// blackHole blocks all data on the connection without closing it. The socket
// remains open, but the server's keep-alive ping is never acknowledged.
func blackHole(t *testing.T, p *toxiclient.Proxy) {
	t.Helper()
	_, err := p.AddToxic(
		"block-data",
		"timeout",
		"upstream",
		1.0, // Probability that the toxic applies.
		toxiclient.Attributes{
			"timeout": 0,
		},
	)
	require.NoError(t, err)
}

// receiveWithin returns the stream's first receive error, or nil if no error
// arrives within the timeout.
func receiveWithin(
	t *testing.T, stream committerpb.Notifier_OpenNotificationStreamClient, timeout time.Duration,
) error {
	t.Helper()

	done := make(chan error, 1)
	go func() {
		_, err := stream.Recv()
		select {
		case done <- err:
		case <-t.Context().Done():
			// The test finished before we could send the error.
		}
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		return nil
	}
}

// freePort returns an unused localhost TCP port.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", localhostDynamicPort)
	require.NoError(t, err)
	defer connection.CloseConnectionsLog(l)
	addr, ok := l.Addr().(*net.TCPAddr)
	require.True(t, ok, "expected TCP address")
	return addr.Port
}
