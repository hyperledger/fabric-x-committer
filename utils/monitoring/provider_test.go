/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package monitoring

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring/promutil"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

type metricsProviderTestEnv struct {
	provider        *Provider
	clientTLSConfig *tls.Config
}

func (e *metricsProviderTestEnv) checkMetrics(t *testing.T, expected ...string) {
	t.Helper()
	test.CheckMetrics(t, e.provider.URL(), e.clientTLSConfig, expected...)
}

func (e *metricsProviderTestEnv) getMetricValue(t *testing.T, metricNameWithLabels string) int {
	t.Helper()
	return test.GetMetricValueFromURL(t, e.provider.URL(), metricNameWithLabels, e.clientTLSConfig)
}

func TestMetricsKindsWithTLSModes(t *testing.T) {
	t.Parallel()

	for _, mode := range test.ServerModes {
		t.Run(fmt.Sprintf("tls-mode:%s", mode), func(t *testing.T) {
			t.Parallel()

			serverTLS, clientTLS := test.CreateServerAndClientTLSConfig(t, mode)
			env := newMetricsProviderTestEnv(t, serverTLS, clientTLS)

			for _, tc := range []struct {
				name string
				test func(*testing.T, *metricsProviderTestEnv)
			}{
				{"Counter", runCounterTest},
				{"CounterVec", runCounterVecTest},
				{"Gauge", runGaugeTest},
				{"GaugeVec", runGaugeVecTest},
				{"Histogram", runHistogramTest},
				{"HistogramVec", runHistogramVecTest},
			} {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()
					tc.test(t, env)
				})
			}
		})
	}
}

func newMetricsProviderTestEnv(t *testing.T, serverTLS, clientTLS connection.TLSConfig) *metricsProviderTestEnv {
	t.Helper()
	p := NewProvider()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)

	c := connection.NewLocalHostServer(serverTLS)
	go func() {
		assert.NoError(t, p.StartPrometheusServer(ctx, c))
	}()

	clientMaterials, err := connection.NewTLSMaterials(clientTLS)
	require.NoError(t, err)
	clientTLSConfig, err := clientMaterials.CreateClientTLSConfig()
	require.NoError(t, err)

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: clientTLSConfig,
		},
	}
	defer client.CloseIdleConnections()
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		require.NotEmpty(ct, p.URL())
		resp, err := client.Get(p.URL())
		require.NoError(ct, err)
		require.NotNil(ct, resp)
		require.Equal(ct, http.StatusOK, resp.StatusCode)
		require.NoError(ct, resp.Body.Close())
	}, 5*time.Second, 100*time.Millisecond)

	return &metricsProviderTestEnv{
		provider:        p,
		clientTLSConfig: clientTLSConfig,
	}
}

func runCounterTest(t *testing.T, env *metricsProviderTestEnv) {
	t.Helper()
	c := env.provider.NewCounter(prometheus.CounterOpts{
		Namespace: "vcservice",
		Subsystem: "committed",
		Name:      "transaction_total",
		Help:      "The total number of transactions committed",
	})

	c.Inc()
	c.Inc()

	env.checkMetrics(t, "vcservice_committed_transaction_total 2")

	promutil.AddToCounter(c, 10)
	env.checkMetrics(t, "vcservice_committed_transaction_total 12")
}

func runCounterVecTest(t *testing.T, env *metricsProviderTestEnv) {
	t.Helper()
	cv := env.provider.NewCounterVec(prometheus.CounterOpts{
		Namespace: "vcservice",
		Subsystem: "preparer",
		Name:      "transaction_total",
		Help:      "Total number of transactions prepared",
	}, []string{"namespace"})

	cv.With(prometheus.Labels{"namespace": "ns_1"}).Inc()
	promutil.AddToCounterVec(cv, []string{"ns_2"}, 1)
	promutil.AddToCounterVec(cv, []string{"ns_1"}, 1)

	env.checkMetrics(t,
		`vcservice_preparer_transaction_total{namespace="ns_1"} 2`,
		`vcservice_preparer_transaction_total{namespace="ns_2"} 1`,
	)
	require.Equal(t, 2,
		env.getMetricValue(t, `vcservice_preparer_transaction_total{namespace="ns_1"}`),
	)
}

func runGaugeTest(t *testing.T, env *metricsProviderTestEnv) {
	t.Helper()
	g := env.provider.NewGauge(prometheus.GaugeOpts{
		Namespace: "vcservice",
		Subsystem: "preparer",
		Name:      "transactions_queued",
		Help:      "Number of transactions waiting to be prepared",
	})

	g.Add(10)
	env.checkMetrics(t, "vcservice_preparer_transactions_queued 10")

	g.Sub(3)
	env.checkMetrics(t, "vcservice_preparer_transactions_queued 7")

	promutil.SetGauge(g, 5)
	env.checkMetrics(t, "vcservice_preparer_transactions_queued 5")
}

func runGaugeVecTest(t *testing.T, env *metricsProviderTestEnv) {
	t.Helper()
	gv := env.provider.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "vcservice",
		Subsystem: "committer",
		Name:      "transactions_queued",
		Help:      "Number of transactions waiting to be committed",
	}, []string{"namespace"})

	gv.With(prometheus.Labels{"namespace": "ns_1"}).Add(7)
	gv.With(prometheus.Labels{"namespace": "ns_2"}).Add(2)
	env.checkMetrics(t,
		`vcservice_committer_transactions_queued{namespace="ns_1"} 7`,
		`vcservice_committer_transactions_queued{namespace="ns_2"} 2`,
	)

	promutil.SetGaugeVec(gv, []string{"ns_1"}, 4)
	env.checkMetrics(t,
		`vcservice_committer_transactions_queued{namespace="ns_1"} 4`,
		`vcservice_committer_transactions_queued{namespace="ns_2"} 2`,
	)
}

func runHistogramTest(t *testing.T, env *metricsProviderTestEnv) {
	t.Helper()
	h := env.provider.NewHistogram(prometheus.HistogramOpts{
		Namespace: "vcservice",
		Subsystem: "committer",
		Name:      "transactions_duration_seconds",
		Help:      "Time taken to commit a batch of transactions",
	})

	h.Observe(500 * time.Millisecond.Seconds())
	h.Observe(time.Second.Seconds())
	promutil.Observe(h, 10*time.Second)
	env.checkMetrics(t,
		`vcservice_committer_transactions_duration_seconds_bucket{le="0.5"} 1`,
		`vcservice_committer_transactions_duration_seconds_bucket{le="1"} 2`,
		`vcservice_committer_transactions_duration_seconds_bucket{le="10"} 3`,
	)
}

func runHistogramVecTest(t *testing.T, env *metricsProviderTestEnv) {
	t.Helper()
	hv := env.provider.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "vcservice",
		Subsystem: "committer",
		Name:      "fetch_versions_duration_seconds",
		Help:      "Time taken to fetch versions from the database",
		Buckets:   []float64{0.5, 0.6, 0.7},
	}, []string{"namespace"})

	hv.With(prometheus.Labels{"namespace": "ns_1"}).Observe(500 * time.Millisecond.Seconds())
	hv.With(prometheus.Labels{"namespace": "ns_2"}).Observe(time.Second.Seconds())
	hv.WithLabelValues("ns_1").Observe(10 * time.Second.Seconds())

	env.checkMetrics(t,
		`vcservice_committer_fetch_versions_duration_seconds_bucket{namespace="ns_1",le="0.5"} 1`,
		`vcservice_committer_fetch_versions_duration_seconds_bucket{namespace="ns_1",le="0.6"} 1`,
		`vcservice_committer_fetch_versions_duration_seconds_bucket{namespace="ns_1",le="0.7"} 1`,
		`vcservice_committer_fetch_versions_duration_seconds_bucket{namespace="ns_1",le="+Inf"} 2`,
		`vcservice_committer_fetch_versions_duration_seconds_bucket{namespace="ns_2",le="0.5"} 0`,
		`vcservice_committer_fetch_versions_duration_seconds_bucket{namespace="ns_2",le="0.6"} 0`,
		`vcservice_committer_fetch_versions_duration_seconds_bucket{namespace="ns_2",le="0.7"} 0`,
		`vcservice_committer_fetch_versions_duration_seconds_bucket{namespace="ns_2",le="+Inf"} 1`,
	)
}
