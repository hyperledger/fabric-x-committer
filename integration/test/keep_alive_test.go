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
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/status"

	"github.com/hyperledger/fabric-x-committer/integration/runner"
)

// blackHoleProxy is a proxy that can silently drop all traffic on a
// connection without closing it.
//
// A live gRPC client cannot be made unresponsive through configuration: its
// transport automatically responds to server pings. To produce a genuinely
// silent client, the connection is routed through a proxy that blocks data transportation.
// The socket remains open, but no bytes flow, so the server's ping
// is never acknowledged and the server must close the connection itself.
type blackHoleProxy struct {
	*toxiclient.Proxy
}

// newBlackHoleProxy creates a proxy control plane between the client and the service.
func newBlackHoleProxy(t *testing.T, upstream string) blackHoleProxy {
	t.Helper()

	proxyAddress := net.JoinHostPort("localhost", strconv.Itoa(freePort(t)))
	server := toxiproxy.NewServer(toxiproxy.NewMetricsContainer(prometheus.NewRegistry()), zerolog.Nop())
	go func() { _ = server.Listen(proxyAddress) }()
	t.Cleanup(func() { _ = server.Shutdown() })

	client := toxiclient.NewClient(proxyAddress)
	require.Eventually(t, func() bool {
		_, err := client.Proxies()
		return err == nil
	}, 15*time.Second, 50*time.Millisecond, "proxy control plane did not start")

	proxy, err := client.CreateProxy(
		"keepalive", net.JoinHostPort("localhost", strconv.Itoa(freePort(t))), upstream,
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = proxy.Delete() })

	return blackHoleProxy{proxy}
}

// blackHole blocks all data on the connection without closing it. The socket
// remains open, but the server's keep-alive ping is never acknowledged.
func (p blackHoleProxy) blackHole(t *testing.T) {
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

const (
	keepAliveTime    = 5 * time.Second
	keepAliveTimeout = 10 * time.Second

	// The server should close the connection within Time and Timeout,
	// but we add 15 seconds so the context will not finish before.
	connectionClosingTime = keepAliveTime + keepAliveTimeout + 15*time.Second
)

// TestSidecarKeepAliveDeadConnectionDetection verifies that the sidecar server detects
// and closes a client connection whose keep-alive pings go unanswered.
func TestSidecarKeepAliveDeadConnectionDetection(t *testing.T) {
	t.Parallel()

	c := runner.NewRuntime(t, &runner.Config{
		BlockTimeout:                 2 * time.Second,
		KeepAliveTime:                keepAliveTime,
		KeepAliveTimeout:             keepAliveTimeout,
		KeepAlivePermitWithoutStream: false,
	})
	c.Start(t, runner.FullTxPath)

	clientCreds, err := c.SystemConfig.ClientTLS.ClientCredentials()
	require.NoError(t, err)
	sidecarAddr := c.SystemConfig.Services.Sidecar.GrpcEndpoint.Address()

	// Route the client through the proxy.
	proxy := newBlackHoleProxy(t, sidecarAddr)

	conn, err := grpc.NewClient(proxy.Listen, grpc.WithTransportCredentials(clientCreds))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	notifyClient := committerpb.NewNotifierClient(conn)
	stream, err := notifyClient.OpenNotificationStream(context.Background())
	require.NoError(t, err)
	require.NoError(t, stream.Send(&committerpb.NotificationRequest{
		TxStatusRequest: &committerpb.TxIDsBatch{TxIds: []string{"dummy-tx"}},
	}))

	// Force the connection to be established.
	conn.Connect()
	require.Eventually(t, func() bool {
		return conn.GetState() == connectivity.Ready
	}, 30*time.Second, 500*time.Millisecond, "connection must be ready before blocking data transport")

	// Block data transport. The socket remains open, but no bytes flow, so the
	// server observes a vanished client rather than a clean disconnect.
	proxy.blackHole(t)

	recvErr := receiveWithin(stream, connectionClosingTime)
	t.Logf("receivedErr: %v", recvErr)
	require.Error(t, recvErr, "server should close the dead connection via keep-alive")

	// Verify that the server-initiated close is translated into a gRPC Unavailable error.
	require.Equal(t, codes.Unavailable, status.Code(recvErr), "expected server-initiated close")
}

// TestQueryKeepAliveDeadConnectionDetection verifies that the query server
// detects and closes a client connection whose keep-alive pings go unanswered.
func TestQueryKeepAliveDeadConnectionDetection(t *testing.T) {
	t.Parallel()

	c := runner.NewRuntime(t, &runner.Config{
		BlockTimeout:                 2 * time.Second,
		KeepAliveTime:                keepAliveTime,
		KeepAliveTimeout:             keepAliveTimeout,
		KeepAlivePermitWithoutStream: true,
	})
	c.Start(t, runner.FullTxPathWithQuery)

	clientCreds, err := c.SystemConfig.ClientTLS.ClientCredentials()
	require.NoError(t, err)
	queryAddr := c.SystemConfig.Services.Query.GrpcEndpoint.Address()

	// Route the client through the proxy.
	proxy := newBlackHoleProxy(t, queryAddr)

	conn, err := grpc.NewClient(proxy.Listen, grpc.WithTransportCredentials(clientCreds))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	queryClient := committerpb.NewQueryServiceClient(conn)

	// Force the connection to be established.
	conn.Connect()
	require.Eventually(t, func() bool {
		return conn.GetState() == connectivity.Ready
	}, 30*time.Second, 500*time.Millisecond, "connection must be ready before the partition")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	_, err = queryClient.GetTransactionStatus(ctx, &committerpb.TxStatusQuery{TxIds: []string{"dummy-tx"}})
	cancel()
	require.NoError(t, err, "query should succeed before the partition")

	// Block data transport. The socket remains open, but no bytes flow, so the
	// server observes a vanished client rather than a clean disconnect.
	proxy.blackHole(t)

	require.Eventually(t, func() bool {
		return conn.GetState() != connectivity.Ready
	}, connectionClosingTime, 200*time.Millisecond,
		"server should close the dead connection via keep-alive")
}

// receiveWithin returns the stream's first receive error, or nil if no error
// arrives within the timeout.
func receiveWithin(stream committerpb.Notifier_OpenNotificationStreamClient, timeout time.Duration) error {
	done := make(chan error, 1)
	go func() {
		_, err := stream.Recv()
		done <- err
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
	l, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)
	defer func() { _ = l.Close() }()
	addr, ok := l.Addr().(*net.TCPAddr)
	require.True(t, ok, "expected TCP address")
	return addr.Port
}
