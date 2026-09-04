/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package monitoring_test

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

func TestChannelLenGaugeVec(t *testing.T) {
	t.Parallel()

	env := newMetricsProviderTestEnv(t, test.InsecureTLSConfig, test.InsecureTLSConfig)

	vec := monitoring.NewChannelLenGaugeVec[int](env.provider, prometheus.GaugeOpts{
		Namespace: "verifier_server",
		Subsystem: "parallel_executor",
		Name:      "stream_input_queue_size",
		Help:      "Size of a stream's queue of transactions waiting to be verified",
	}, []string{"stream"})

	// With nothing registered the collector exports no series at all, rather than a zero.
	require.NotContains(t, env.getMetrics(t), "verifier_server_parallel_executor_stream_input_queue_size")

	first, second := make(chan int, 4), make(chan int, 4)
	vec.Register(first, "1")
	vec.Register(second, "2")
	first <- 1
	first <- 2
	second <- 1

	// Each queue is its own series, so an operator can sum them or read one worker.
	env.checkMetrics(
		t,
		`verifier_server_parallel_executor_stream_input_queue_size{stream="1"} 2`,
		`verifier_server_parallel_executor_stream_input_queue_size{stream="2"} 1`,
	)

	<-first
	env.checkMetrics(t, `verifier_server_parallel_executor_stream_input_queue_size{stream="1"} 1`)

	// A stream that ends takes its series with it. Were it left behind it would report the depth
	// of a queue nothing drains any more, which reads as a stuck worker.
	vec.Unregister("1")
	metrics := env.getMetrics(t)
	require.NotContains(t, metrics, `stream_input_queue_size{stream="1"}`)
	require.Contains(t, metrics, `verifier_server_parallel_executor_stream_input_queue_size{stream="2"} 1`)

	// Unregistering something that was never registered is a no-op.
	require.NotPanics(t, func() { vec.Unregister("nonexistent") })
}

func TestChannelLenGaugeVecReRegister(t *testing.T) {
	t.Parallel()

	env := newMetricsProviderTestEnv(t, test.InsecureTLSConfig, test.InsecureTLSConfig)

	vec := monitoring.NewChannelLenGaugeVec[int](env.provider, prometheus.GaugeOpts{
		Namespace: "sidecar",
		Subsystem: "notifier",
		Name:      "stream_block_queue_size",
		Help:      "Size of one stream's queue of blocks waiting to be sent",
	}, []string{"stream"})

	// Re-registering the same label values replaces the channel rather than exporting the label
	// twice, which prometheus would reject as a duplicate series mid-scrape.
	old := make(chan int, 4)
	old <- 1
	old <- 2
	vec.Register(old, "1")
	env.checkMetrics(t, `sidecar_notifier_stream_block_queue_size{stream="1"} 2`)

	fresh := make(chan int, 4)
	fresh <- 1
	vec.Register(fresh, "1")
	env.checkMetrics(t, `sidecar_notifier_stream_block_queue_size{stream="1"} 1`)
}
