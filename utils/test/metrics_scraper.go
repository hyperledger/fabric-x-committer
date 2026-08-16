/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"crypto/tls"
	"github.com/hyperledger/fabric-x-committer/integration/runner"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
)

// MetricsScraper scrapes a service's active-connections gauge from its metrics endpoint.
type MetricsScraper struct {
	url       string
	tlsConfig *tls.Config
}

// NewMetricsScraper builds a handle for scraping the named active-connections gauge on the
// given service's HTTP metrics endpoint.
func NewMetricsScraper(
	t *testing.T, c *runner.CommitterRuntime, httpEndpoint *connection.Endpoint,
) MetricsScraper {
	t.Helper()

	metricsURL, err := monitoring.MakeMetricsURL(httpEndpoint.Address(), &c.SystemConfig.ClientTLS)
	require.NoError(t, err)

	creds, err := connection.NewClientTLSCredentials(c.SystemConfig.ClientTLS)
	require.NoError(t, err)

	tlsConfig, err := creds.CreateClientTLSConfig()
	require.NoError(t, err)

	return MetricsScraper{
		url:       metricsURL,
		tlsConfig: tlsConfig,
	}
}

// Value returns the gauge's current Value.
func (s MetricsScraper) Value(t TestingT, metricName string) int {
	t.Helper()
	return GetMetricValueFromURL(t, GetMetricValueParameters{
		URL:        s.url,
		MetricName: metricName,
		TLSConfig:  s.tlsConfig,
	})
}

// ValueWithLabels returns the current ValueWithLabels of the named series carrying the given labels, or 0 if the
// series is not present yet.
func (s MetricsScraper) ValueWithLabels(t TestingT, metricName string, labels map[string]string) int {
	t.Helper()
	v, _ := GetLabeledMetricValueFromURL(t, GetMetricValueParameters{
		URL:        s.url,
		MetricName: metricName,
		Labels:     labels,
		TLSConfig:  s.tlsConfig,
	})
	return v
}
