/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"fmt"
	"testing"
	"time"

	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/integration/runner"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

func TestLoadGenWithTLSModes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		serviceFlags int
	}{
		{
			name:         "orderer with committer",
			serviceFlags: runner.FullTxPathWithLoadGen,
		},
		{
			name:         "only orderer",
			serviceFlags: runner.LoadGenForOnlyOrderer | runner.Orderer,
		},
		{
			name:         "committer",
			serviceFlags: runner.CommitterTxPathWithLoadGen,
		},
		{
			name:         "coordinator",
			serviceFlags: runner.LoadGenForCoordinator | runner.Coordinator | runner.VC | runner.Verifier,
		},
		{
			name:         "VC",
			serviceFlags: runner.LoadGenForVCService | runner.VC,
		},
		{
			name:         "verifier",
			serviceFlags: runner.LoadGenForVerifier | runner.Verifier,
		},
		{
			name:         "verifier with distributed",
			serviceFlags: runner.LoadGenForVerifier | runner.LoadGenForDistributedLoadGen | runner.Verifier,
		},
	} {
		serviceFlags := tc.serviceFlags
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, mode := range test.ServerModes {
				t.Run(fmt.Sprintf("tls-mode:%s", mode), func(t *testing.T) {
					t.Parallel()
					c := runner.NewRuntime(t, &runner.Config{
						NumVerifiers: 2,
						NumVCService: 2,
						BlockTimeout: 2 * time.Second,
						BlockSize:    100,
						TLSMode:      mode,
					})
					c.Start(t, serviceFlags)

					metricsCreds, err := connection.NewClientTLSCredentials(c.SystemConfig.ClientTLS)
					require.NoError(t, err)
					metricsClientTLSConfig, err := metricsCreds.CreateClientTLSConfig()
					require.NoError(t, err)

					metricsURL, err := monitoring.MakeMetricsURL(
						c.SystemConfig.Services.LoadGen.HTTPEndpoint.Address(), &c.SystemConfig.ClientTLS,
					)
					require.NoError(t, err)
					require.EventuallyWithT(t, func(ct *assert.CollectT) {
						count := test.GetCounterOrGaugeValueFromURL(ct, test.GetMetricValueParameters{
							URL:        metricsURL,
							MetricName: "loadgen_transaction_committed_total",
							TLSConfig:  metricsClientTLSConfig,
						})
						t.Logf("loadgen_transaction_committed_total: %d", count)
						require.Greater(ct, count, 500)
					}, 300*time.Second, 1*time.Second)
				})
			}
		})
	}
}

func TestLoadGenCommitterWithLimit(t *testing.T) {
	t.Parallel()
	c := runner.NewRuntime(t, &runner.Config{
		BlockTimeout: 2 * time.Second,
		BlockSize:    500,
	})
	c.SystemConfig.LoadGenBlockLimit = 4
	c.Start(t, runner.CommitterTxPathWithLoadGen)

	expectedTXs := 500*3 + 2 // +2 for config and namespace TXs
	require.Eventually(t, func() bool {
		count := c.CountStatus(t, committerpb.Status_COMMITTED)
		t.Logf("count %d", count)
		return count >= expectedTXs
	}, 90*time.Second, 1*time.Second)
	require.Zero(t, c.CountAlternateStatus(t, committerpb.Status_COMMITTED))

	count := c.CountStatus(t, committerpb.Status_COMMITTED)
	require.Equal(t, expectedTXs, count)
}

// TestLoadGenWithQueryService runs the committer path and a query service together with a loadgen
// configured for queries-rate > 0, so the loadgen commits transactions while concurrently fetching
// committed read versions from the query service before signing (the "load query and committer in
// parallel" case). It asserts both loads actually flow: the loadgen keeps committing, and the query
// service keeps receiving key queries. Version hits are not required — with key-backref-rate at its
// default (0) every read is a fresh key, so queries miss and reads keep nil versions; this exercises
// the wiring and the
// query service under a live commit load.
func TestLoadGenWithQueryService(t *testing.T) {
	t.Parallel()
	c := runner.NewRuntime(t, &runner.Config{
		NumVerifiers:       2,
		NumVCService:       2,
		BlockTimeout:       2 * time.Second,
		BlockSize:          100,
		LoadGenQueriesRate: 1,
	})
	c.Start(t, runner.CommitterTxPathWithLoadGen|runner.QueryService)

	metricsCreds, err := connection.NewClientTLSCredentials(c.SystemConfig.ClientTLS)
	require.NoError(t, err)
	metricsClientTLSConfig, err := metricsCreds.CreateClientTLSConfig()
	require.NoError(t, err)

	loadGenMetricsURL, err := monitoring.MakeMetricsURL(
		c.SystemConfig.Services.LoadGen.HTTPEndpoint.Address(), &c.SystemConfig.ClientTLS,
	)
	require.NoError(t, err)
	queryMetricsURL, err := monitoring.MakeMetricsURL(
		c.SystemConfig.Services.Query.HTTPEndpoint.Address(), &c.SystemConfig.ClientTLS,
	)
	require.NoError(t, err)

	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		committed := test.GetMetricValueFromURL(
			ct, loadGenMetricsURL, "loadgen_transaction_committed_total", metricsClientTLSConfig,
		)
		queried := test.GetMetricValueFromURL(
			ct, queryMetricsURL, "queryservice_grpc_key_requested_total", metricsClientTLSConfig,
		)
		t.Logf("loadgen_transaction_committed_total=%d queryservice_grpc_key_requested_total=%d", committed, queried)
		require.Greater(ct, committed, 500)
		require.Positive(ct, queried)
	}, 300*time.Second, 1*time.Second)
}
