/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package monitoring

import (
	"context"
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
	provider *Provider
}

func newMetricsProviderTestEnv(t *testing.T) *metricsProviderTestEnv {
	t.Helper()
	p := NewProvider()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)

	c := connection.NewLocalHostServer(test.InsecureTLSConfig)
	go func() {
		assert.NoError(t, p.StartPrometheusServer(ctx, c))
	}()

	client := &http.Client{}
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
		provider: p,
	}
}

func TestCounter(t *testing.T) {
	t.Parallel()

	env := newMetricsProviderTestEnv(t)

	opts := prometheus.CounterOpts{
		Namespace: "vcservice",
		Subsystem: "committed",
		Name:      "transaction_total",
		Help:      "The total number of transactions committed",
	}
	c := env.provider.NewCounter(opts)

	c.Inc()
	c.Inc()

	test.CheckMetrics(t, env.provider.url, "vcservice_committed_transaction_total 2")

	promutil.AddToCounter(c, 10)
	test.CheckMetrics(t, env.provider.url, "vcservice_committed_transaction_total 12")
}

func TestCounterVec(t *testing.T) {
	t.Parallel()

	env := newMetricsProviderTestEnv(t)

	opts := prometheus.CounterOpts{
		Namespace: "vcservice",
		Subsystem: "preparer",
		Name:      "transaction_total",
		Help:      "Total number of transactions prepared",
	}
	labels := []string{"namespace"}
	cv := env.provider.NewCounterVec(opts, labels)

	cv.With(prometheus.Labels{"namespace": "ns_1"}).Inc()
	promutil.AddToCounterVec(cv, []string{"ns_2"}, 1)
	promutil.AddToCounterVec(cv, []string{"ns_1"}, 1)

	test.CheckMetrics(t, env.provider.url,
		`vcservice_preparer_transaction_total{namespace="ns_1"} 2`,
		`vcservice_preparer_transaction_total{namespace="ns_2"} 1`,
	)

	v := test.GetMetricValueFromURL(
		t, env.provider.url, "vcservice_preparer_transaction_total{namespace=\"ns_1\"}",
	)
	require.Equal(t, 2, v)
}

func TestNewGuage(t *testing.T) {
	t.Parallel()

	env := newMetricsProviderTestEnv(t)

	opts := prometheus.GaugeOpts{
		Namespace: "vcservice",
		Subsystem: "preparer",
		Name:      "transactions_queued",
		Help:      "Number of transactions waiting to be prepared",
	}
	g := env.provider.NewGauge(opts)

	g.Add(10)
	test.CheckMetrics(t, env.provider.url, "vcservice_preparer_transactions_queued 10")

	g.Sub(3)
	test.CheckMetrics(t, env.provider.url, "vcservice_preparer_transactions_queued 7")

	promutil.SetGauge(g, 5)
	test.CheckMetrics(t, env.provider.url, "vcservice_preparer_transactions_queued 5")
}

func TestNewGuageVec(t *testing.T) {
	t.Parallel()

	env := newMetricsProviderTestEnv(t)

	opts := prometheus.GaugeOpts{
		Namespace: "vcservice",
		Subsystem: "committer",
		Name:      "transactions_queued",
		Help:      "Number of transactions waiting to be committed",
	}
	gv := env.provider.NewGaugeVec(opts, []string{"namespace"})

	gv.With(prometheus.Labels{"namespace": "ns_1"}).Add(7)
	gv.With(prometheus.Labels{"namespace": "ns_2"}).Add(2)
	test.CheckMetrics(
		t,
		env.provider.url,
		`vcservice_committer_transactions_queued{namespace="ns_1"} 7`,
		`vcservice_committer_transactions_queued{namespace="ns_2"} 2`,
	)

	promutil.SetGaugeVec(gv, []string{"ns_1"}, 4)
	test.CheckMetrics(
		t,
		env.provider.url,
		`vcservice_committer_transactions_queued{namespace="ns_1"} 4`,
		`vcservice_committer_transactions_queued{namespace="ns_2"} 2`,
	)
}

func TestNewHistogram(t *testing.T) {
	t.Parallel()

	env := newMetricsProviderTestEnv(t)

	opts := prometheus.HistogramOpts{
		Namespace: "vcservice",
		Subsystem: "committer",
		Name:      "transactions_duration_seconds",
		Help:      "Time taken to commit a batch of transactions",
	}
	h := env.provider.NewHistogram(opts)

	h.Observe(500 * time.Millisecond.Seconds())
	h.Observe(time.Second.Seconds())
	promutil.Observe(h, 10*time.Second)
	test.CheckMetrics(
		t,
		env.provider.url,
		`vcservice_committer_transactions_duration_seconds_bucket{le="0.5"} 1`,
		`vcservice_committer_transactions_duration_seconds_bucket{le="1"} 2`,
		`vcservice_committer_transactions_duration_seconds_bucket{le="10"} 3`,
	)
}

func TestNewHistogramVec(t *testing.T) {
	t.Parallel()

	env := newMetricsProviderTestEnv(t)

	opts := prometheus.HistogramOpts{
		Namespace: "vcservice",
		Subsystem: "committer",
		Name:      "fetch_versions_duration_seconds",
		Help:      "Time taken to fetch versions from the database",
		Buckets:   []float64{0.5, 0.6, 0.7},
	}
	h := env.provider.NewHistogramVec(opts, []string{"namespace"})

	h.With(prometheus.Labels{"namespace": "ns_1"}).Observe(500 * time.Millisecond.Seconds())
	h.With(prometheus.Labels{"namespace": "ns_2"}).Observe(time.Second.Seconds())
	h.WithLabelValues("ns_1").Observe(10 * time.Second.Seconds())

	test.CheckMetrics(
		t,
		env.provider.url,
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
