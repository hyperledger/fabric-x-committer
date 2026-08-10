/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package serve_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
	"github.com/hyperledger/fabric-x-committer/utils/serve"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

type connStatsRegisterer struct {
	serverMetrics *serve.ServerMetrics
}

func (r *connStatsRegisterer) RegisterService(srv serve.Servers) {
	serve.RegisterServerMetrics(srv.StatsHandler, r.serverMetrics)
}

// TestServerConnStatsHandler verifies the active-connections gauge end to end:
// wired through the normal RegisterService path, it rises as real clients connect
// and returns to zero as they disconnect.
func TestServerConnStatsHandler(t *testing.T) {
	t.Parallel()

	t.Log("Starting service")

	serverStatsMetrics := serve.NewServerMetrics(monitoring.NewProvider(), monitoring.MetricsParameters{
		Namespace: "test",
		Subsystem: "conn_stats",
	})

	serverConfig := test.NewLocalHostServiceConfig(test.InsecureTLSConfig)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)
	test.ServeForTest(ctx, t, serverConfig, &connStatsRegisterer{
		serverMetrics: serverStatsMetrics,
	})

	t.Log("Creating clients")
	conn := test.NewInsecureConnection(t, &serverConfig.GRPC.Endpoint)
	conn2 := test.NewInsecureConnection(t, &serverConfig.GRPC.Endpoint)

	test.RequireIntMetricValue(t, 0, serve.GetActiveConnections(serverStatsMetrics))

	t.Log("Connecting clients")
	conn.Connect()
	test.EventuallyIntMetric(t, 1, serve.GetActiveConnections(serverStatsMetrics), 30*time.Second, 100*time.Millisecond)
	conn2.Connect()
	test.EventuallyIntMetric(t, 2, serve.GetActiveConnections(serverStatsMetrics), 30*time.Second, 100*time.Millisecond)

	t.Log("Disconnecting clients")
	require.NoError(t, conn.Close())
	require.NoError(t, conn2.Close())
	test.EventuallyIntMetric(t, 0, serve.GetActiveConnections(serverStatsMetrics), 30*time.Second, 100*time.Millisecond)
}
