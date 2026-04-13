/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package monitoring

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/utils/test"
)

func TestHTTPServer_ServesMetrics(t *testing.T) {
	t.Parallel()
	provider := NewMetricsProvider()
	// Create a counter to verify it appears in metrics
	counter := provider.NewCounter(prometheus.CounterOpts{
		Name: "test_metric_total",
		Help: "Test metric for HTTPServer",
	})
	counter.Inc()

	testConfig := test.NewLocalHostServer(test.InsecureTLSConfig)
	testConfig.Endpoint.Port = 0 // Let OS assign ephemeral port

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	// Use StartHTTPServer which creates the listener synchronously (returning any errors)
	// and then starts serving in a goroutine. We need the URL before serving starts.
	server := newHTTPServer(testConfig, provider.Registry())
	listener, err := server.createListener(ctx)
	require.NoError(t, err, "failed to create listener")

	// Construct the metrics URL using the helper
	url := makeMetricsURL(t, testConfig, listener)

	// Start serving in a goroutine (simulating what StartHTTPServer does internally)
	go server.serve(ctx, listener)

	// Wait for server to be ready
	require.Eventually(t, func() bool {
		resp, reqErr := http.Get(url) //nolint:gosec // test code making request to localhost
		if reqErr == nil {
			_ = resp.Body.Close()
			return resp.StatusCode == http.StatusOK
		}
		return false
	}, 5*time.Second, 100*time.Millisecond, "server did not start")

	// Actually make HTTP request to verify metrics are served
	resp, err := http.Get(url) //nolint:gosec // test code making request to localhost
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	// Verify our test metric appears in the response
	assert.Contains(t, string(body), "test_metric")

	// Cleanup
	cancel()
}
