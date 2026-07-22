/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"context"
	"google.golang.org/grpc/keepalive"
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
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/hyperledger/fabric-x-committer/integration/runner"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

const (
	keepAliveTime    = 5 * time.Second
	keepAliveTimeout = 10 * time.Second

	// The server should close the connection within Time + Timeout,
	// but we allow some headroom to avoid racing the context timeout.
	maxConnectionClosingTime = keepAliveTime + keepAliveTimeout + 2*time.Minute

	localhostDynamicPort = "localhost:0"
	dummyTxID            = "dummy-tx"

	sidecarService = iota
	queryService
)

// keepAliveConfig holds the per-test knobs for a keep-alive runtime.
type keepAliveConfig struct {
	permitWithoutStream  bool
	maxConcurrentStreams int
}

// TestKeepAliveDeadConnectionDetection verifies that a service's server-side keep-alive
// closes a silent (black-holed) connection on its own.
func TestKeepAliveDeadConnectionDetection(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name                string
		service             int
		permitWithoutStream bool
	}{
		{
			name:                "Sidecar",
			service:             sidecarService,
			permitWithoutStream: false,
		},
		{
			name:                "Query",
			service:             queryService,
			permitWithoutStream: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			c := startKeepAliveRuntime(t, keepAliveConfig{permitWithoutStream: tc.permitWithoutStream})
			proxy, conn := dialThroughProxy(t, serviceAddr(t, c, tc.service), clientCredentials(t, c))

			sendInitialMessage(t, conn, tc.service)

			interceptMessages(t, proxy)

			require.EventuallyWithT(t, func(ct *assert.CollectT) {
				require.NotEqual(ct, connectivity.Ready, conn.GetState())
			}, maxConnectionClosingTime, 200*time.Millisecond)
		})
	}
}

// TestKeepAliveSidecarStreamSlotRelease verifies that once keep-alive closes a dead
// connection, the concurrent-stream slot it held is released for new clients.
func TestKeepAliveSidecarStreamSlotRelease(t *testing.T) {
	t.Parallel()

	c := startKeepAliveRuntime(t, keepAliveConfig{
		permitWithoutStream:  false,
		maxConcurrentStreams: 4,
	})

	addr := serviceAddr(t, c, sidecarService)
	clientCreds := clientCredentials(t, c)

	proxy, conn := dialThroughProxy(t, addr, clientCreds)

	sendInitialMessage(t, conn, sidecarService)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	t.Cleanup(cancel)

	conn2, err := grpc.NewClient(addr, grpc.WithTransportCredentials(clientCreds))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn2.Close() })

	client2 := committerpb.NewNotifierClient(conn2)
	stream2, err := client2.OpenNotificationStream(ctx)
	if err == nil {
		_, err = stream2.Recv()
	}
	require.Error(t, err, "second stream should fail with ResourceExhausted")

	interceptMessages(t, proxy)

	// Once keep-alive closes the dead connection, the slot is released.
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		stream3, err := client2.OpenNotificationStream(ctx)
		require.NoError(ct, err)
		require.NoError(ct, stream3.Send(&committerpb.NotificationRequest{
			TxStatusRequest: &committerpb.TxIDsBatch{TxIds: []string{dummyTxID}},
			Timeout:         durationpb.New(2 * time.Second),
		}))

		_, err = stream3.Recv()
		require.NoError(ct, err, "third stream should be admitted after the slot is released")
	}, maxConnectionClosingTime, 500*time.Millisecond)
}

func startKeepAliveRuntime(t *testing.T, cfg keepAliveConfig) *runner.CommitterRuntime {
	t.Helper()
	c := runner.NewRuntime(t, &runner.Config{
		BlockTimeout:                 2 * time.Second,
		KeepAliveTime:                keepAliveTime,
		KeepAliveTimeout:             keepAliveTimeout,
		KeepAlivePermitWithoutStream: cfg.permitWithoutStream,
		MaxConcurrentStreams:         cfg.maxConcurrentStreams,
	})
	c.Start(t, runner.FullTxPathWithQuery)
	return c
}

// clientCredentials returns the TLS credentials a client uses to reach the runtime services.
func clientCredentials(t *testing.T, c *runner.CommitterRuntime) credentials.TransportCredentials {
	t.Helper()
	creds, err := c.SystemConfig.ClientTLS.ClientCredentials()
	require.NoError(t, err)
	return creds
}

// serviceAddr returns the endpoint address of the given service in the runtime.
func serviceAddr(t *testing.T, c *runner.CommitterRuntime, service int) string {
	t.Helper()
	switch service {
	case sidecarService:
		return c.SystemConfig.Services.Sidecar.GrpcEndpoint.Address()
	case queryService:
		return c.SystemConfig.Services.Query.GrpcEndpoint.Address()
	default:
		require.FailNow(t, "unknown service kind")
		return ""
	}
}

// sendInitialMessage issues an RPC on a connection so the server has traffic to monitor with keep-alive.
func sendInitialMessage(t *testing.T, conn *grpc.ClientConn, service int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	t.Cleanup(cancel)

	switch service {
	case sidecarService:
		stream, err := committerpb.NewNotifierClient(conn).OpenNotificationStream(ctx)
		require.NoError(t, err)
		require.NoError(t, stream.Send(&committerpb.NotificationRequest{
			TxStatusRequest: &committerpb.TxIDsBatch{TxIds: []string{dummyTxID}},
		}))
	case queryService:
		_, err := committerpb.NewQueryServiceClient(conn).GetTransactionStatus(ctx, &committerpb.TxStatusQuery{
			TxIds: []string{dummyTxID},
		})
		require.NoError(t, err)
	default:
		require.FailNow(t, "unknown service kind")
	}
}

// dialThroughProxy routes a connection through a toxiproxy that can later black-hole the traffic.
func dialThroughProxy(
	t *testing.T, serviceAddr string, clientCreds credentials.TransportCredentials,
) (*toxiclient.Proxy, *grpc.ClientConn) {
	t.Helper()
	proxy := newProxy(t, serviceAddr)

	conn, err := grpc.NewClient(proxy.Listen,
		grpc.WithTransportCredentials(clientCreds), grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:    999 * time.Hour, // effectively disable client-side keep-alive
			Timeout: 999 * time.Hour, // effectively disable client-side keep-alive
		}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	conn.Connect()
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		require.Equal(ct, connectivity.Ready, conn.GetState())
	}, 30*time.Second, 500*time.Millisecond)

	return proxy, conn
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

// interceptMessages blocks all data on the connection without closing it (only client -> server). The socket
// remains open, but the server's keep-alive ping is never acknowledged.
func interceptMessages(t *testing.T, p *toxiclient.Proxy) {
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
