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

// GaugeOrCounterValue returns the current value of the named counter/gauge/untyped series carrying the given
// labels, rounded to the nearest integer. Pass nil labels to sum every series in the family. It
// returns 0 for a labeled series not exported yet.
func (s MetricsScraper) GaugeOrCounterValue(t TestingT, metricName string, labels map[string]string) int {
	t.Helper()
	return GetCounterOrGaugeValueFromURL(t, GetMetricValueParameters{
		URL:        s.url,
		MetricName: metricName,
		Labels:     labels,
		TLSConfig:  s.tlsConfig,
	})
}

// HistogramCountAndSumValue returns the observation count and sum of the named histogram carrying the given
// labels, rounded to the nearest integer. Pass the histogram's base name, not its "_count" child.
func (s MetricsScraper) HistogramCountAndSumValue(
	t TestingT, metricName string, labels map[string]string,
) (uint64, float64) {
	t.Helper()
	return GetHistogramCountAndSumValueFromURL(t, GetMetricValueParameters{
		URL:        s.url,
		MetricName: metricName,
		Labels:     labels,
		TLSConfig:  s.tlsConfig,
	})
}
