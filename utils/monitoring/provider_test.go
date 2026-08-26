/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package monitoring_test

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring/promutil"
	"github.com/hyperledger/fabric-x-committer/utils/serve"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

type metricsProviderTestEnv struct {
	provider        *monitoring.Provider
	url             string
	clientTLSConfig *tls.Config
}

func TestCounterWithTLSModes(t *testing.T) {
	t.Parallel()

	for _, mode := range test.ServerModes {
		t.Run(fmt.Sprintf("tls-mode:%s", mode), func(t *testing.T) {
			t.Parallel()
			serverTLS, clientTLS := test.CreateServerAndClientTLSConfig(t, mode)
			env := newMetricsProviderTestEnv(t, serverTLS, clientTLS)

			opts := prometheus.CounterOpts{
				Namespace: "vcservice",
				Subsystem: "committed",
				Name:      "transaction_total",
				Help:      "The total number of transactions committed",
			}
			c := env.provider.NewCounter(opts)

			c.Inc()
			c.Inc()

			env.checkMetrics(t, "vcservice_committed_transaction_total 2")

			promutil.AddToCounter(c, 10)
			env.checkMetrics(t, "vcservice_committed_transaction_total 12")
		})
	}
}

func TestCounterVec(t *testing.T) {
	t.Parallel()

	env := newMetricsProviderTestEnv(t, test.InsecureTLSConfig, test.InsecureTLSConfig)

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

	env.checkMetrics(
		t,
		`vcservice_preparer_transaction_total{namespace="ns_1"} 2`,
		`vcservice_preparer_transaction_total{namespace="ns_2"} 1`,
	)
	require.Equal(t, 2, test.GetCounterOrGaugeValueFromURL(t, test.GetMetricValueParameters{
		URL:        env.url,
		MetricName: "vcservice_preparer_transaction_total",
		Labels:     map[string]string{"namespace": "ns_1"},
		TLSConfig:  env.clientTLSConfig,
	}))
}

func TestNewGuage(t *testing.T) {
	t.Parallel()

	env := newMetricsProviderTestEnv(t, test.InsecureTLSConfig, test.InsecureTLSConfig)

	opts := prometheus.GaugeOpts{
		Namespace: "vcservice",
		Subsystem: "preparer",
		Name:      "transactions_queued",
		Help:      "Number of transactions waiting to be prepared",
	}
	g := env.provider.NewGauge(opts)

	g.Add(10)
	env.checkMetrics(t, "vcservice_preparer_transactions_queued 10")

	g.Sub(3)
	env.checkMetrics(t, "vcservice_preparer_transactions_queued 7")

	promutil.SetGauge(g, 5)
	env.checkMetrics(t, "vcservice_preparer_transactions_queued 5")
}

func TestNewChannelLenGauge(t *testing.T) {
	t.Parallel()

	env := newMetricsProviderTestEnv(t, test.InsecureTLSConfig, test.InsecureTLSConfig)

	ch := make(chan int, 3)
	g := env.provider.NewChannelLenGauge(prometheus.GaugeOpts{
		Namespace: "vcservice",
		Subsystem: "preparer",
		Name:      "input_queue_size",
		Help:      "Number of batches waiting to be prepared",
	}, ch)

	env.checkMetrics(t, "vcservice_preparer_input_queue_size 0")

	// The value is read on scrape, so it tracks the channel without anything setting it.
	ch <- 1
	ch <- 2
	env.checkMetrics(t, "vcservice_preparer_input_queue_size 2")

	<-ch
	env.checkMetrics(t, "vcservice_preparer_input_queue_size 1")

	// A nil channel reports zero rather than panicking on the scrape path.
	var nilCh chan int
	nilGauge := env.provider.NewChannelLenGauge(prometheus.GaugeOpts{
		Namespace: "vcservice",
		Subsystem: "validator",
		Name:      "pending_queue_size",
		Help:      "Number of batches waiting to be validated",
	}, nilCh)
	require.Equal(t, 0, test.GetIntMetricValue(t, nilGauge))
	require.Equal(t, 1, test.GetIntMetricValue(t, g))
}

func TestNewAtomicChannelLenGauge(t *testing.T) {
	t.Parallel()

	env := newMetricsProviderTestEnv(t, test.InsecureTLSConfig, test.InsecureTLSConfig)

	var holder atomic.Pointer[chan int]
	g := env.provider.NewAtomicChannelLenGauge(prometheus.GaugeOpts{
		Namespace: "sidecar",
		Subsystem: "relay",
		Name:      "input_block_queue_size",
		Help:      "Number of blocks waiting to be relayed",
	}, &holder)

	// An unset pointer reports zero rather than dereferencing nil.
	env.checkMetrics(t, "sidecar_relay_input_block_queue_size 0")

	first := make(chan int, 5)
	holder.Store(&first)
	first <- 1
	first <- 2
	env.checkMetrics(t, "sidecar_relay_input_block_queue_size 2")

	// Replacing the channel must move the gauge to the new one instead of latching the value of
	// the channel the previous session left behind.
	second := make(chan int, 5)
	holder.Store(&second)
	env.checkMetrics(t, "sidecar_relay_input_block_queue_size 0")

	second <- 1
	require.Equal(t, 1, test.GetIntMetricValue(t, g))
	require.Len(t, first, 2)
}

func TestNewGuageVec(t *testing.T) {
	t.Parallel()

	env := newMetricsProviderTestEnv(t, test.InsecureTLSConfig, test.InsecureTLSConfig)

	opts := prometheus.GaugeOpts{
		Namespace: "vcservice",
		Subsystem: "committer",
		Name:      "transactions_queued",
		Help:      "Number of transactions waiting to be committed",
	}
	gv := env.provider.NewGaugeVec(opts, []string{"namespace"})

	gv.With(prometheus.Labels{"namespace": "ns_1"}).Add(7)
	gv.With(prometheus.Labels{"namespace": "ns_2"}).Add(2)
	env.checkMetrics(
		t, `vcservice_committer_transactions_queued{namespace="ns_1"} 7`,
		`vcservice_committer_transactions_queued{namespace="ns_2"} 2`,
	)

	promutil.SetGaugeVec(gv, []string{"ns_1"}, 4)
	env.checkMetrics(
		t, `vcservice_committer_transactions_queued{namespace="ns_1"} 4`,
		`vcservice_committer_transactions_queued{namespace="ns_2"} 2`,
	)
}

func TestNewHistogram(t *testing.T) {
	t.Parallel()

	env := newMetricsProviderTestEnv(t, test.InsecureTLSConfig, test.InsecureTLSConfig)

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
	env.checkMetrics(
		t,
		`vcservice_committer_transactions_duration_seconds_bucket{le="0.5"} 1`,
		`vcservice_committer_transactions_duration_seconds_bucket{le="1"} 2`,
		`vcservice_committer_transactions_duration_seconds_bucket{le="10"} 3`,
	)
}

func TestNewHistogramVec(t *testing.T) {
	t.Parallel()

	env := newMetricsProviderTestEnv(t, test.InsecureTLSConfig, test.InsecureTLSConfig)

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

	env.checkMetrics(
		t,
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

func TestPprofEndpoints(t *testing.T) {
	t.Parallel()

	env := newMetricsProviderTestEnv(t, test.InsecureTLSConfig, test.InsecureTLSConfig)

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: env.clientTLSConfig,
		},
	}
	defer client.CloseIdleConnections()

	// Extract base URL from metrics URL (remove /metrics path)
	metricsURL := env.url
	baseURL := metricsURL[:len(metricsURL)-len(monitoring.MetricsSubPath)]

	tests := []struct {
		name string
		path string
	}{
		{name: "Index", path: "/debug/pprof/"},
		{name: "Cmdline", path: "/debug/pprof/cmdline"},
		{name: "Profile", path: "/debug/pprof/profile?seconds=1"},
		{name: "Symbol", path: "/debug/pprof/symbol"},
		{name: "Heap", path: "/debug/pprof/heap"},
		{name: "Goroutine", path: "/debug/pprof/goroutine"},
		{name: "Allocs", path: "/debug/pprof/allocs"},
		{name: "Block", path: "/debug/pprof/block"},
		{name: "Mutex", path: "/debug/pprof/mutex"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			resp, err := client.Get(baseURL + tt.path)
			require.NoError(t, err)
			require.NotNil(t, resp)
			require.Equal(t, http.StatusOK, resp.StatusCode)
			require.NoError(t, resp.Body.Close())
		})
	}
}

type fakeService struct {
	*monitoring.Provider
}

func (f *fakeService) RegisterService(s serve.Servers) {
	monitoring.RegisterMonitoringServer(s.HTTP, f.Provider)
}

func newMetricsProviderTestEnv(t *testing.T, serverTLS, clientTLS connection.TLSConfig) *metricsProviderTestEnv {
	t.Helper()
	p := monitoring.NewProvider()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)

	serverConfig := test.NewLocalHostServiceConfig(serverTLS)
	test.ServeForTest(ctx, t, serverConfig, &fakeService{Provider: p})

	clientCreds, err := connection.NewClientTLSCredentials(clientTLS)
	require.NoError(t, err)
	clientTLSConfig, err := clientCreds.CreateClientTLSConfig()
	require.NoError(t, err)

	metricsURL, err := monitoring.MakeMetricsURL(serverConfig.HTTP.Endpoint.Address(), &serverTLS)
	require.NoError(t, err)

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: clientTLSConfig,
		},
	}
	defer client.CloseIdleConnections()

	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		resp, err := client.Get(metricsURL)
		require.NoError(ct, err)
		require.NotNil(ct, resp)
		require.Equal(ct, http.StatusOK, resp.StatusCode)
		require.NoError(ct, resp.Body.Close())
	}, 5*time.Second, 100*time.Millisecond)

	return &metricsProviderTestEnv{
		provider:        p,
		url:             metricsURL,
		clientTLSConfig: clientTLSConfig,
	}
}

func (e *metricsProviderTestEnv) checkMetrics(t *testing.T, expected ...string) {
	t.Helper()
	test.CheckMetrics(t, e.url, e.clientTLSConfig, expected...)
}
