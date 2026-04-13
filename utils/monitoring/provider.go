/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package monitoring

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
)

// MetricsProvider is a prometheus metrics provider.
type MetricsProvider struct {
	registry *prometheus.Registry
}

// NewMetricsProvider creates a new prometheus metrics provider.
func NewMetricsProvider() *MetricsProvider {
	return &MetricsProvider{registry: prometheus.NewRegistry()}
}

// Registry returns the prometheus registry.
func (p *MetricsProvider) Registry() *prometheus.Registry {
	return p.registry
}

// NewCounter creates a new prometheus counter.
func (p *MetricsProvider) NewCounter(opts prometheus.CounterOpts) prometheus.Counter {
	c := prometheus.NewCounter(opts)
	p.registry.MustRegister(c)
	return c
}

// NewCounterVec creates a new prometheus counter vector.
func (p *MetricsProvider) NewCounterVec(opts prometheus.CounterOpts, labels []string) *prometheus.CounterVec {
	cv := prometheus.NewCounterVec(opts, labels)
	p.registry.MustRegister(cv)
	return cv
}

// NewGauge creates a new prometheus gauge.
func (p *MetricsProvider) NewGauge(opts prometheus.GaugeOpts) prometheus.Gauge {
	g := prometheus.NewGauge(opts)
	p.registry.MustRegister(g)
	return g
}

// NewGaugeVec creates a new prometheus gauge vector.
func (p *MetricsProvider) NewGaugeVec(opts prometheus.GaugeOpts, labels []string) *prometheus.GaugeVec {
	gv := prometheus.NewGaugeVec(opts, labels)
	p.registry.MustRegister(gv)
	return gv
}

// NewHistogram creates a new prometheus histogram.
func (p *MetricsProvider) NewHistogram(opts prometheus.HistogramOpts) prometheus.Histogram {
	h := prometheus.NewHistogram(opts)
	p.registry.MustRegister(h)
	return h
}

// NewHistogramVec creates a new prometheus histogram vector.
func (p *MetricsProvider) NewHistogramVec(opts prometheus.HistogramOpts, labels []string) *prometheus.HistogramVec {
	hv := prometheus.NewHistogramVec(opts, labels)
	p.registry.MustRegister(hv)
	return hv
}

// NewThroughputCounter creates a new prometheus throughput counter.
func (p *MetricsProvider) NewThroughputCounter(
	component, subComponent string,
	direction ThroughputDirection,
) prometheus.Counter {
	return p.NewCounter(prometheus.CounterOpts{
		Namespace: component,
		Subsystem: subComponent,
		Name:      fmt.Sprintf("%s_throughput", direction),
		Help:      "Incoming requests/Outgoing responses for a component",
	})
}

// NewConnectionMetrics supports common connection metrics.
func (p *MetricsProvider) NewConnectionMetrics(opts ConnectionMetricsOpts) *ConnectionMetrics {
	subsystem := fmt.Sprintf("grpc_%s", opts.RemoteNamespace)
	return &ConnectionMetrics{
		Status: p.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: opts.Namespace,
			Subsystem: subsystem,
			Name:      "connection_status",
			Help: fmt.Sprintf(
				"Connection status to %s service by grpc target (1 = connected, 0 = disconnected).",
				opts.RemoteNamespace,
			),
		}, []string{"grpc_target"}),
		FailureTotal: p.NewCounterVec(prometheus.CounterOpts{
			Namespace: opts.Namespace,
			Subsystem: subsystem,
			Name:      "connection_failure_total",
			Help: fmt.Sprintf("Total number of connection failures to %s service.", opts.RemoteNamespace) +
				"Short-lived failures may not always be captured.",
		}, []string{"grpc_target"}),
	}
}
