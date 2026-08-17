/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"crypto/tls"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
)

// MetricsScraper scrapes named metric series from a service's HTTP metrics endpoint. The metric to
// read is chosen per call, so one scraper serves every metric a service exposes.
type MetricsScraper struct {
	url       string
	tlsConfig *tls.Config
}

// NewMetricsScraper builds a handle for scraping the given service's HTTP metrics endpoint.
func NewMetricsScraper(
	t *testing.T, clientTLS connection.TLSConfig, httpEndpoint *connection.Endpoint,
) MetricsScraper {
	t.Helper()

	metricsURL, err := monitoring.MakeMetricsURL(httpEndpoint.Address(), &clientTLS)
	require.NoError(t, err)

	creds, err := connection.NewClientTLSCredentials(clientTLS)
	require.NoError(t, err)

	tlsConfig, err := creds.CreateClientTLSConfig()
	require.NoError(t, err)

	return MetricsScraper{
		url:       metricsURL,
		tlsConfig: tlsConfig,
	}
}

// Value returns the current value of the named series, failing the test if it is not exported yet.
func (s MetricsScraper) Value(t TestingT, metricName string) int {
	t.Helper()
	return GetMetricValueFromURL(t, GetMetricValueParameters{
		URL:        s.url,
		MetricName: metricName,
		TLSConfig:  s.tlsConfig,
	})
}

// ValueWithLabels returns the current value of the named series carrying the given labels.
func (s MetricsScraper) ValueWithLabels(t TestingT, metricName string, labels map[string]string) int {
	t.Helper()
	return GetMetricValueFromURL(t, GetMetricValueParameters{
		URL:        s.url,
		MetricName: metricName,
		Labels:     labels,
		TLSConfig:  s.tlsConfig,
	})
}

// FloatValueWithLabels returns the unrounded current value of the named series carrying the given
// labels, or 0 if the series is not present yet. Use this over ValueWithLabels for sub-integer
// values, such as a histogram _sum of short durations, that must not be rounded to zero.
func (s MetricsScraper) FloatValueWithLabels(t TestingT, metricName string, labels map[string]string) float64 {
	t.Helper()
	value, _ := findFloatMetricValueFromURL(t, GetMetricValueParameters{
		URL:        s.url,
		MetricName: metricName,
		Labels:     labels,
		TLSConfig:  s.tlsConfig,
	})
	return value
}
