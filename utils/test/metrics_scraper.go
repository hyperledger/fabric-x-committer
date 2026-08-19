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
	URL       string
	TLSConfig *tls.Config
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
		URL:       metricsURL,
		TLSConfig: tlsConfig,
	}
}
