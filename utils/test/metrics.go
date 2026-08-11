/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"crypto/tls"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	promgo "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
)

// GetMetricValueParameters is used to pass parameters to GetMetricValueFromURL and GetLabeledMetricValueFromURL.
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

// GetMetricValueFromURL reads the metrics endpoint and fetch the value of a specific metric.
func GetMetricValueFromURL(t TestingT, params GetMetricValueParameters) int {
	t.Helper()
	metricsOutput := getMetricsFromURL(t, params.URL, params.TLSConfig)
	r, err := regexp.Compile(`(?m)^` + params.MetricName + `\s+([\d.]+)`)
	require.NoError(t, err)
	m := r.FindStringSubmatch(metricsOutput)
	// Without this, a metric that is not exported yet indexes an empty match and panics -- which,
	// from a polling goroutine, takes the whole test binary down.
	require.Lenf(t, m, 2, "metric [%s] not found", params.MetricName)
	val, err := strconv.ParseFloat(m[1], 64)
	require.NoError(t, err)
	return int(math.Round(val))
}

// GetLabeledMetricValueFromURL reads the metrics endpoint and returns the value of the metric
// series named metricName carrying the given labels. Only the given labels must match; the series
// may carry additional labels.
func GetLabeledMetricValueFromURL(
	t TestingT, params GetMetricValueParameters,
) (int, bool) {
	t.Helper()

	seriesRegex := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(params.MetricName) + `\{([^}]*)\}\s+(\S+)`)

	for _, m := range seriesRegex.FindAllStringSubmatch(getMetricsFromURL(t, params.URL, params.TLSConfig), -1) {
		if !labelsMatch(m[1], params.Labels) {
			continue
		}
		val, err := strconv.ParseFloat(m[2], 64)
		require.NoError(t, err)
		return int(math.Round(val)), true
	}
	return 0, false
}

// labelsMatch reports whether the prometheus label segment (e.g. `method="x",status="OK"`)
// contains every requested key="value" pair.
func labelsMatch(labelSegment string, labels map[string]string) bool {
	for k, v := range labels {
		if !strings.Contains(labelSegment, fmt.Sprintf("%s=%q", k, v)) {
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
