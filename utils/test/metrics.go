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
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	promgo "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// CheckMetrics checks the metrics endpoint for the expected metrics.
func CheckMetrics(t *testing.T, url string, tlsConfig *tls.Config, expectedMetrics ...string) {
	t.Helper()
	metricsOutput := getMetricsFromURL(t, url, tlsConfig)
	for _, expected := range expectedMetrics {
		require.Contains(t, metricsOutput, expected)
	}
}

// GetMetricValueFromURL reads the metrics endpoint and fetch the value of a specific metric.
func GetMetricValueFromURL(t *testing.T, url, metricName string, tlsConfig *tls.Config) int {
	t.Helper()
	metricsOutput := getMetricsFromURL(t, url, tlsConfig)
	r, err := regexp.Compile(`(?m)^` + metricName + `\s+([\d.]+)`)
	require.NoError(t, err)
	m := r.FindStringSubmatch(metricsOutput)
	val, err := strconv.ParseFloat(m[1], 64)
	require.NoError(t, err)
	return int(math.Round(val))
}

func getMetricsFromURL(t *testing.T, url string, tlsConfig *tls.Config) string {
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
		require.NoError(t, err)
		require.NoError(ct, resp.Body.Close())
		val = string(b)
	}, time.Minute, 100*time.Millisecond)
	return val
}

// GetMetricValue returns the value of a prometheus metric.
func GetMetricValue(t *testing.T, m prometheus.Metric) float64 {
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
		return gm.Histogram.GetSampleSum()
	default:
		require.Fail(t, "unsupported metric")
		return 0
	}
}

// GetIntMetricValue returns the value of a prometheus metric, rounded to the nearest integer.
func GetIntMetricValue(t *testing.T, m prometheus.Metric) int {
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
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		v := GetIntMetricValue(t, m)
		require.Equal(c, expected, v)
	}, waitFor, tick, msgAndArgs...)
}
