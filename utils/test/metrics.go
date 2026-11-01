/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"crypto/tls"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	promgo "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
)

type (
	// MetricsConnectionParameters carries the scrape URL and client TLS config for a service's
	// HTTP metrics endpoint.
	MetricsConnectionParameters struct {
		URL       string
		TLSConfig *tls.Config
	}

	// GetMetricValueParameters carries the parameters for the metric-value lookups
	// (GetCounterOrGaugeValueFromURL, GetHistogramCountAndSumValueFromURL).
	GetMetricValueParameters struct {
		MetricsConnectionParameters
		MetricName string
		Labels     map[string]string
	}
)

// NewMetricsConnectionParameters builds a handle for scraping the given service's HTTP metrics endpoint.
func NewMetricsConnectionParameters(
	t *testing.T, clientTLS connection.TLSConfig, httpEndpoint *connection.Endpoint,
) MetricsConnectionParameters {
	t.Helper()

	metricsURL, err := monitoring.MakeMetricsURL(httpEndpoint.Address(), &clientTLS)
	require.NoError(t, err)

	creds, err := connection.NewClientTLSCredentials(clientTLS)
	require.NoError(t, err)

	tlsConfig, err := creds.CreateClientTLSConfig()
	require.NoError(t, err)

	return MetricsConnectionParameters{
		URL:       metricsURL,
		TLSConfig: tlsConfig,
	}
}

// CheckMetrics checks the metrics endpoint for the expected metrics.
func CheckMetrics(t *testing.T, url string, tlsConfig *tls.Config, expectedMetrics ...string) {
	t.Helper()
	metricsOutput := getMetricsFromURL(t, url, tlsConfig)
	for _, expected := range expectedMetrics {
		require.Contains(t, metricsOutput, expected)
	}
}

// GetCounterOrGaugeValueFromURL reads the metrics endpoint and returns the scalar value of the
// counter/gauge/untyped series named params.MetricName carrying params.Labels, rounded to the
// nearest integer. It returns 0 for an absent family and for a labeled series not exported yet (a
// "...Vec" series is absent until its first observation, so a pre-traffic baseline read returns 0).
// Reading a histogram/summary this way also returns 0 -- use GetHistogramCountAndSumValueFromURL.
func GetCounterOrGaugeValueFromURL(t TestingT, params GetMetricValueParameters) int {
	t.Helper()
	var sum float64

	for _, m := range getMetricSeries(t, params) {
		switch {
		// Branch on which typed field is set: GetValue returns 0 for a nil field, so the value alone
		// cannot tell a real zero from the wrong metric kind.
		case m.Counter != nil:
			sum += m.Counter.GetValue()
		case m.Gauge != nil:
			sum += m.Gauge.GetValue()
		default:
			sum += m.Untyped.GetValue()
		}
	}
	return int(math.Round(sum))
}

// GetHistogramCountAndSumValueFromURL reads the metrics endpoint and returns the observation count and the sum of the
// histogram named params.MetricName carrying params.Labels. Pass the histogram's base name, not its
// "_count" child. Absent-family and not-yet-observed reads return 0; a
// non-histogram family also reads as 0.
func GetHistogramCountAndSumValueFromURL(t TestingT, params GetMetricValueParameters) (count uint64, sum float64) {
	t.Helper()

	for _, m := range getMetricSeries(t, params) {
		count += m.Histogram.GetSampleCount()
		sum += m.Histogram.GetSampleSum()
	}
	return count, sum
}

// getMetricSeries fetches and parses the exposition text and returns every series of the family
// named params.MetricName that carries params.Labels. It returns no series (so callers sum to 0)
// when the family is absent: a labeled series is not exported until its first observation, and a
// missing unlabeled family reads the same way.
func getMetricSeries(
	t TestingT, params GetMetricValueParameters,
) []*promgo.Metric {
	t.Helper()
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(strings.NewReader(getMetricsFromURL(t, params.URL, params.TLSConfig)))
	require.NoError(t, err)

	family := families[params.MetricName]
	if family == nil {
		return nil
	}

	series := make([]*promgo.Metric, 0, len(family.Metric))
	for _, m := range family.GetMetric() {
		if labelsMatch(m, params.Labels) {
			series = append(series, m)
		}
	}
	return series
}

// labelsMatch reports whether the series carries every requested key/value label pair. Matching is
// exact per label (name and value).
func labelsMatch(m *promgo.Metric, want map[string]string) bool {
	have := make(map[string]string, len(m.GetLabel()))
	for _, l := range m.GetLabel() {
		have[l.GetName()] = l.GetValue()
	}
	for name, value := range want {
		if have[name] != value {
			return false
		}
	}
	return true
}

func getMetricsFromURL(t TestingT, url string, tlsConfig *tls.Config) string {
	t.Helper()
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: tlsConfig,
		},
	}
	defer client.CloseIdleConnections()
	var val string
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		resp, err := client.Get(url)
		require.NoError(ct, err)
		require.NotNil(ct, resp)
		require.Equal(ct, http.StatusOK, resp.StatusCode)
		b, err := io.ReadAll(resp.Body)
		require.NoError(ct, err)
		require.NoError(ct, resp.Body.Close())
		val = string(b)
	}, time.Minute, 100*time.Millisecond)
	return val
}

// GetMetricValue returns the value of a prometheus metric.
func GetMetricValue(t TestingT, m prometheus.Metric) float64 {
	t.Helper()
	gm := promgo.Metric{}
	require.NoError(t, m.Write(&gm))

	switch {
	case gm.Gauge != nil:
		return gm.Gauge.GetValue()
	case gm.Counter != nil:
		return gm.Counter.GetValue()
	case gm.Untyped != nil:
		return gm.Untyped.GetValue()
	case gm.Summary != nil:
		return gm.Summary.GetSampleSum()
	case gm.Histogram != nil:
		count := gm.Histogram.GetSampleCount()
		// A histogram child with no observations would divide 0/0 = NaN, which testify cannot order.
		// Return 0 so callers asserting positivity fail cleanly instead.
		if count == 0 {
			return 0
		}
		return gm.Histogram.GetSampleSum() / float64(count)
	default:
		require.Fail(t, "unsupported metric")
		return 0
	}
}

// GetIntMetricValue returns the value of a prometheus metric, rounded to the nearest integer.
func GetIntMetricValue(t TestingT, m prometheus.Metric) int {
	t.Helper()
	val := GetMetricValue(t, m)
	return int(math.Round(val))
}

// RequireIntMetricValue fail the test if the integer metric is not equal to the expected value.
func RequireIntMetricValue(t *testing.T, expected int, m prometheus.Metric) {
	t.Helper()
	require.Equal(t, expected, GetIntMetricValue(t, m))
}

// EventuallyIntMetric fail the test if the integer metric is not equal to the expected value after the given duration.
func EventuallyIntMetric( //nolint:revive // number of arguments is derived from the [require] package.
	t *testing.T, expected int, m prometheus.Metric, waitFor, tick time.Duration, msgAndArgs ...any,
) {
	t.Helper()
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		v := GetIntMetricValue(ct, m)
		require.Equal(ct, expected, v)
	}, waitFor, tick, msgAndArgs...)
}

// ExpectedConn is used to describe the expected connection state.
type ExpectedConn struct {
	Status       int
	FailureTotal int
}

// RequireConnectionMetrics waits for a connection status and a specified number of failures.
func RequireConnectionMetrics(
	t *testing.T,
	label string,
	connMetrics *monitoring.ConnectionMetrics,
	expected ExpectedConn,
) {
	t.Helper()
	connStatus, err := connMetrics.Status.GetMetricWithLabelValues(label)
	require.NoError(t, err)
	connFailure, err := connMetrics.FailureTotal.GetMetricWithLabelValues(label)
	require.NoError(t, err)

	EventuallyIntMetric(t, expected.Status, connStatus, 30*time.Second, 200*time.Millisecond)
	RequireIntMetricValue(t, expected.FailureTotal, connFailure)
	RequireIntMetricValue(t, expected.Status, connStatus)
}

// WaitForConnections waits for a connection metric to have the required number of connected labels.
func WaitForConnections(tb testing.TB, p *monitoring.Provider, name string, requiredCount int) {
	tb.Helper()
	require.Eventually(tb, func() bool {
		gather, err := p.Registry().Gather()
		require.NoError(tb, err)
		connectedCount := 0
		for _, mf := range gather {
			if mf.GetName() != name {
				continue
			}
			for _, m := range mf.GetMetric() {
				val := m.GetGauge().GetValue()
				if math.Abs(val-connection.Connected) < 1e-10 {
					connectedCount++
				}
			}
		}
		return connectedCount >= requiredCount
	}, time.Minute, 10*time.Millisecond)
}
