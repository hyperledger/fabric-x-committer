/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"context"
	"crypto/tls"
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
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/hyperledger/fabric-x-committer/integration/runner"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

const (
	keepAliveTime    = 5 * time.Second
	keepAliveTimeout = 10 * time.Second

	// The server should close the connection within Time + Timeout,
	// but we allow some headroom to avoid racing the context timeout.
	maxConnectionClosingTime = keepAliveTime + keepAliveTimeout + 2*time.Minute

	localhostDynamicPort = "localhost:0"
	dummyTxID            = "dummy-tx"

	sidecarActiveConnectionsMetric = "sidecar_grpc_active_connections"
	queryActiveConnectionsMetric   = "queryservice_grpc_active_connections"
)

type (
	// keepAliveConfig holds the per-test knobs for a keep-alive runtime.
	keepAliveConfig struct {
		permitWithoutStream  bool
		maxConcurrentStreams int
	}

	// metricsGetter scrapes a service's active-connections gauge from its metrics endpoint.
	metricsGetter struct {
		url       string
		name      string
		tlsConfig *tls.Config
	}
)

// value returns the gauge's current value. It takes a [test.TestingT] so a polling condition can
// pass its [assert.CollectT] instead of the test's own T.
func (g metricsGetter) value(t test.TestingT) int {
	t.Helper()
	return test.GetMetricValueFromURL(t, g.url, g.name, g.tlsConfig)
}

// TestKeepAliveSidecarDeadConnectionDetection verifies that the sidecar's server-side keep-alive
// closes a silent connection on its own. It observes the close directly on the server, through
// its active-connections metric.
func TestKeepAliveSidecarDeadConnectionDetection(t *testing.T) {
	t.Parallel()

	c := startKeepAliveRuntime(t, keepAliveConfig{permitWithoutStream: false})

	gauge := newMetricsGetter(
		t, c, c.SystemConfig.Services.Sidecar.HTTPEndpoint, sidecarActiveConnectionsMetric,
	)

	// Number of connections the Sidecar before our client connects.
	prevActiveConnections := gauge.value(t)

	proxy, conn := dialThroughProxy(
		t, c.SystemConfig.Services.Sidecar.GrpcEndpoint.Address(), clientCredentials(t, c),
	)

	sendSidecarInitialMessage(t, conn)

	blockAndWaitForServerClose(t, proxy, gauge, prevActiveConnections)
}

// TestKeepAliveQueryDeadConnectionDetection verifies that the query service's server-side keep-alive
// closes a silent connection on its own. It observes the close directly on the server, through
// its active-connections metric.
func TestKeepAliveQueryDeadConnectionDetection(t *testing.T) {
	t.Parallel()

	c := startKeepAliveRuntime(t, keepAliveConfig{permitWithoutStream: true})

	gauge := newMetricsGetter(
		t, c, c.SystemConfig.Services.Query.HTTPEndpoint, queryActiveConnectionsMetric,
	)

	// Number of connections the Query before our client connects.
	prevActiveConnections := gauge.value(t)

	proxy, conn := dialThroughProxy(
		t, c.SystemConfig.Services.Query.GrpcEndpoint.Address(), clientCredentials(t, c),
	)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	t.Cleanup(cancel)

	_, err := committerpb.NewQueryServiceClient(conn).GetTransactionStatus(ctx, &committerpb.TxStatusQuery{
		TxIds: []string{dummyTxID},
	})
	require.NoError(t, err)

	blockAndWaitForServerClose(t, proxy, gauge, prevActiveConnections)
}

// TestKeepAliveSidecarStreamSlotRelease verifies that once keep-alive closes a dead
// connection, the concurrent-stream slot it held is released for new clients. It also
// confirms, through the server's active-connections metric, that the server itself
// closed the dead connection.
func TestKeepAliveSidecarStreamSlotRelease(t *testing.T) {
	t.Parallel()

	c := startKeepAliveRuntime(t, keepAliveConfig{
		permitWithoutStream: false,
		// maxConcurrentStreams is sized so the runtime's own sidecar streams + this
		// test's stream fill every slot, so the next OpenNotificationStream is rejected.
		maxConcurrentStreams: 4,
	})

	addr := c.SystemConfig.Services.Sidecar.GrpcEndpoint.Address()
	clientCreds := clientCredentials(t, c)
	gauge := newMetricsGetter(
		t, c, c.SystemConfig.Services.Sidecar.HTTPEndpoint, sidecarActiveConnectionsMetric,
	)

	// Number of connections the Sidecar before our client connects.
	prevActiveConnections := gauge.value(t)

	proxy, conn := dialThroughProxy(t, addr, clientCreds)

	sendSidecarInitialMessage(t, conn)

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
	require.Error(t, err)
	require.Equal(t, codes.ResourceExhausted, status.Code(err),
		"second stream should be rejected with ResourceExhausted")

	// Both the intercepted connection and conn2 are now open on the server.
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		require.Equal(ct, prevActiveConnections+2, gauge.value(ct))
	}, 30*time.Second, 200*time.Millisecond)

	blockMessages(t, proxy)

	// Keep-alive closes the dead connection: the server reports one fewer active
	// connection (conn2 stays open).
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		require.Equal(ct, prevActiveConnections+1, gauge.value(ct))
	}, maxConnectionClosingTime, 500*time.Millisecond)

	// And the stream slot it held is released for new clients.
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

// newMetricsGetter builds a handle for scraping the named active-connections gauge on the
// given service's HTTP metrics endpoint.
func newMetricsGetter(
	t *testing.T, c *runner.CommitterRuntime, httpEndpoint *connection.Endpoint, metricName string,
) metricsGetter {
	t.Helper()

	metricsURL, err := monitoring.MakeMetricsURL(httpEndpoint.Address(), &c.SystemConfig.ClientTLS)
	require.NoError(t, err)

	creds, err := connection.NewClientTLSCredentials(c.SystemConfig.ClientTLS)
	require.NoError(t, err)

	tlsConfig, err := creds.CreateClientTLSConfig()
	require.NoError(t, err)

	return metricsGetter{
		url:       metricsURL,
		name:      metricName,
		tlsConfig: tlsConfig,
	}
}

// blockAndWaitForServerClose intercepts the proxied connection and asserts, through the
// service's active-connections gauge, that the server first counts the new connection and then,
// once its keep-alive detects the now-silent connection, closes it itself — returning the count
// to the number of previous active connections.
func blockAndWaitForServerClose(t *testing.T, proxy *toxiclient.Proxy, gauge metricsGetter, prevActiveConnections int) {
	t.Helper()

	// The server counts the new connection.
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		require.Equal(ct, prevActiveConnections+1, gauge.value(ct))
	}, 30*time.Second, 200*time.Millisecond)

	blockMessages(t, proxy)

	// The server's keep-alive detects the silent connection and closes it itself,
	// so the active-connection count returns to the baseline.
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		require.Equal(ct, prevActiveConnections, gauge.value(ct))
	}, maxConnectionClosingTime, 500*time.Millisecond)
}

// sendSidecarInitialMessage opens a notification stream so the sidecar has traffic to monitor with keep-alive.
func sendSidecarInitialMessage(t *testing.T, conn *grpc.ClientConn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	t.Cleanup(cancel)

	stream, err := committerpb.NewNotifierClient(conn).OpenNotificationStream(ctx)
	require.NoError(t, err)
	require.NoError(t, stream.Send(&committerpb.NotificationRequest{
		TxStatusRequest: &committerpb.TxIDsBatch{TxIds: []string{dummyTxID}},
	}))
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

// blockMessages blocks all data on the connection without closing it (only client -> server). The socket
// remains open, but the server's keep-alive ping is never acknowledged.
func blockMessages(t *testing.T, p *toxiclient.Proxy) {
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
