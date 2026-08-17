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

// GetMetricValueParameters is used to pass parameters to GetMetricValueFromURL.
type GetMetricValueParameters struct {
	MetricName string
	URL        string
	TLSConfig  *tls.Config
	Labels     map[string]string
}

// CheckMetrics checks the metrics endpoint for the expected metrics.
func CheckMetrics(t *testing.T, url string, tlsConfig *tls.Config, expectedMetrics ...string) {
	t.Helper()
	metricsOutput := getMetricsFromURL(t, url, tlsConfig)
	for _, expected := range expectedMetrics {
		require.Contains(t, metricsOutput, expected)
	}
}

// GetMetricValueFromURL reads the metrics endpoint and returns the value of the series named
// params.MetricName carrying params.Labels, rounded to the nearest integer, failing the test if no
// such series is exported yet. Use findFloatMetricValueFromURL for sub-integer values (e.g. a
// histogram _sum of short durations) that must not be rounded to zero.
func GetMetricValueFromURL(t TestingT, params GetMetricValueParameters) int {
	t.Helper()
	value := findFloatMetricValueFromURL(t, params)
	return int(math.Round(value))
}

// findFloatMetricValueFromURL parses the exposition text and sums the float values of every series
// matching params.MetricName and params.Labels. It returns the sum and whether any series matched.
func findFloatMetricValueFromURL(t TestingT, params GetMetricValueParameters) float64 {
	t.Helper()
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(strings.NewReader(getMetricsFromURL(t, params.URL, params.TLSConfig)))
	require.NoError(t, err)

	family, extract := resolveFamily(families, params.MetricName)
	if family == nil {
		return 0
	}

	var sum float64
	for _, m := range family.GetMetric() {
		if labelsMatch(m, params.Labels) {
			sum += extract(m)
		}
	}
	return sum
}

// resolveFamily finds the metric family for name and returns it together with a function that
// extracts the wanted value from one of its series. A histogram/summary is exposed by the parser
// under its base name, so a request for the "<base>_count" or "<base>_sum" child series resolves to
// that family and reads the corresponding aggregate instead of a plain sample value.
func resolveFamily(
	families map[string]*promgo.MetricFamily, name string,
) (*promgo.MetricFamily, func(*promgo.Metric) float64) {
	if family := families[name]; family != nil {
		return family, sampleValue
	}
	if base, ok := strings.CutSuffix(name, "_count"); ok {
		if family := families[base]; family != nil {
			return family, aggregateSampleCount
		}
	}
	if base, ok := strings.CutSuffix(name, "_sum"); ok {
		if family := families[base]; family != nil {
			return family, aggregateSampleSum
		}
	}
	return nil, nil
}

// sampleValue returns the scalar value of a counter, gauge, or untyped series.
func sampleValue(m *promgo.Metric) float64 {
	switch {
	case m.Counter != nil:
		return m.Counter.GetValue()
	case m.Gauge != nil:
		return m.Gauge.GetValue()
	default:
		return m.Untyped.GetValue()
	}
}

// aggregateSampleCount returns the observation count of a histogram or summary series.
func aggregateSampleCount(m *promgo.Metric) float64 {
	if m.Histogram != nil {
		return float64(m.Histogram.GetSampleCount())
	}
	return float64(m.Summary.GetSampleCount())
}

// aggregateSampleSum returns the observation sum of a histogram or summary series.
func aggregateSampleSum(m *promgo.Metric) float64 {
	if m.Histogram != nil {
		return m.Histogram.GetSampleSum()
	}
	return m.Summary.GetSampleSum()
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
		return gm.Histogram.GetSampleSum() / float64(gm.Histogram.GetSampleCount())
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
func WaitForConnections(t *testing.T, p *monitoring.Provider, name string, requiredCount int) {
	t.Helper()
	require.Eventually(t, func() bool {
		gather, err := p.Registry().Gather()
		require.NoError(t, err)
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
