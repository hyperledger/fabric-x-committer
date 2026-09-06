/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package verifier

import (
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
	"github.com/hyperledger/fabric-x-committer/utils/serve"
)

const (
	namespace = "verifier_server"

	subsystemParallelExecutor = "parallel_executor"
)

type metrics struct {
	*monitoring.Provider
	verifierServerTxs *monitoring.ThroughputMetrics
	serverMetrics     *serve.ServerMetrics
	activeRequests    prometheus.Gauge

	// A parallel executor, and therefore its queues, exists per stream. The queues are reported
	// per stream and summed by the operator, e.g.
	// sum(verifier_server_parallel_executor_input_queue_size).
	executorInputQueueSize        *monitoring.ChannelLenGaugeVec[*servicepb.TxWithRef]
	executorOutputSingleQueueSize *monitoring.ChannelLenGaugeVec[*verificationOutput]
	executorOutputQueueSize       *monitoring.ChannelLenGaugeVec[[]*committerpb.TxStatus]
}

func newMonitoring() *metrics {
	p := monitoring.NewProvider()
	streamLabels := []string{"stream"}
	return &metrics{
		Provider: p,
		executorInputQueueSize: monitoring.NewChannelLenGaugeVec[*servicepb.TxWithRef](
			p, prometheus.GaugeOpts{
				Namespace: namespace,
				Subsystem: subsystemParallelExecutor,
				Name:      "input_queue_size",
				Help:      "Size of a stream's queue of transactions waiting to be verified.",
			}, streamLabels,
		),
		executorOutputSingleQueueSize: monitoring.NewChannelLenGaugeVec[*verificationOutput](
			p, prometheus.GaugeOpts{
				Namespace: namespace,
				Subsystem: subsystemParallelExecutor,
				Name:      "output_single_queue_size",
				Help:      "Size of a stream's queue of verified transactions waiting to be batched.",
			}, streamLabels,
		),
		executorOutputQueueSize: monitoring.NewChannelLenGaugeVec[[]*committerpb.TxStatus](
			p, prometheus.GaugeOpts{
				Namespace: namespace,
				Subsystem: subsystemParallelExecutor,
				Name:      "output_queue_size",
				Help:      "Size of a stream's queue of status batches waiting to be sent.",
			}, streamLabels,
		),
		verifierServerTxs: monitoring.NewThroughputMetrics(p, monitoring.MetricsParameters{
			Namespace: namespace,
			Subsystem: "tx",
		}),
		serverMetrics: serve.NewServerMetrics(p, monitoring.MetricsParameters{
			Namespace: namespace,
			Subsystem: "grpc",
		}),
		activeRequests: p.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystemParallelExecutor,
			Name:      "active_requests",
			Help:      "The total number of active requests",
		}),
	}
}

// registerExecutorQueues starts reporting one stream's executor queues. The caller must
// unregister them when the stream ends, otherwise the series keeps reporting a queue nothing
// drains.
func (m *metrics) registerExecutorQueues(streamID string, e *parallelExecutor) {
	m.executorInputQueueSize.Register(e.inputCh, streamID)
	m.executorOutputSingleQueueSize.Register(e.outputSingleCh, streamID)
	m.executorOutputQueueSize.Register(e.outputCh, streamID)
}

// unregisterExecutorQueues removes the series of a stream that has ended.
func (m *metrics) unregisterExecutorQueues(streamID string) {
	m.executorInputQueueSize.Unregister(streamID)
	m.executorOutputSingleQueueSize.Unregister(streamID)
	m.executorOutputQueueSize.Unregister(streamID)
}
