/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package monitoring

import (
	"fmt"
	"net/http"
	"net/http/pprof"
	"net/url"
	"sync/atomic"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-lib-go/common/flogging"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

const (
	httpsScheme    = "https://"
	httpScheme     = "http://"
	metricsSubPath = "/metrics"
	pprofSubPath   = "/debug/pprof/"
)

// Provider is a prometheus metrics provider.
type Provider struct {
	registry *prometheus.Registry
	url      atomic.Pointer[string]
}

var logger = flogging.MustGetLogger("monitoring")

// NewProvider creates a new prometheus metrics provider.
func NewProvider() *Provider {
	return &Provider{registry: prometheus.NewRegistry()}
}

// RegisterMonitoringServer registers the monitoring endpoints with the provided HTTP mux.
// It sets up Prometheus metrics endpoint and pprof profiling handlers.
// It uses the similar signature as GRPC server registration to allow common
// language for all server<->service interfaces.
func RegisterMonitoringServer(mux *http.ServeMux, p *Provider) {
	// Register pprof handlers for profiling.
	// Note: We must explicitly register these handlers because we're using a custom ServeMux.
	// The net/http/pprof package's init() function only registers handlers on http.DefaultServeMux,
	// which we're not using. Simply importing the package is insufficient for custom muxes.
	//
	// The Index handler dynamically serves all runtime profiles (heap, goroutine, allocs, block,
	// mutex, threadcreate, etc.) without requiring explicit registration. Only special handlers
	// (cmdline, profile, symbol, trace) need to be registered separately.
	mux.HandleFunc(pprofSubPath, pprof.Index)
	mux.HandleFunc(pprofSubPath+"cmdline", pprof.Cmdline)
	mux.HandleFunc(pprofSubPath+"profile", pprof.Profile)
	mux.HandleFunc(pprofSubPath+"symbol", pprof.Symbol)
	mux.HandleFunc(pprofSubPath+"trace", pprof.Trace)

	// Register metrics handlers.
	mux.Handle(metricsSubPath, promhttp.HandlerFor(
		p.Registry(), promhttp.HandlerOpts{Registry: p.Registry()},
	))
}

// NewCounter creates a new prometheus counter.
func (p *Provider) NewCounter(opts prometheus.CounterOpts) prometheus.Counter {
	c := prometheus.NewCounter(opts)
	p.registry.MustRegister(c)
	return c
}

// NewCounterVec creates a new prometheus counter vector.
func (p *Provider) NewCounterVec(opts prometheus.CounterOpts, labels []string) *prometheus.CounterVec {
	cv := prometheus.NewCounterVec(opts, labels)
	p.registry.MustRegister(cv)
	return cv
}

// NewGauge creates a new prometheus gauge.
func (p *Provider) NewGauge(opts prometheus.GaugeOpts) prometheus.Gauge {
	g := prometheus.NewGauge(opts)
	p.registry.MustRegister(g)
	return g
}

// NewGaugeVec creates a new prometheus gauge vector.
func (p *Provider) NewGaugeVec(opts prometheus.GaugeOpts, labels []string) *prometheus.GaugeVec {
	gv := prometheus.NewGaugeVec(opts, labels)
	p.registry.MustRegister(gv)
	return gv
}

// NewHistogram creates a new prometheus histogram.
func (p *Provider) NewHistogram(opts prometheus.HistogramOpts) prometheus.Histogram {
	h := prometheus.NewHistogram(opts)
	p.registry.MustRegister(h)
	return h
}

// NewHistogramVec creates a new prometheus histogram vector.
func (p *Provider) NewHistogramVec(opts prometheus.HistogramOpts, labels []string) *prometheus.HistogramVec {
	hv := prometheus.NewHistogramVec(opts, labels)
	p.registry.MustRegister(hv)
	return hv
}

// NewThroughputCounter creates a new prometheus throughput counter.
func (p *Provider) NewThroughputCounter(
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
func (p *Provider) NewConnectionMetrics(opts ConnectionMetricsOpts) *ConnectionMetrics {
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

// Registry returns the prometheus registry.
func (p *Provider) Registry() *prometheus.Registry {
	return p.registry
}

// MakeMetricsURL construct the Prometheus metrics URL.
// based on the secure level, we set the url scheme to http or https.
func MakeMetricsURL(address string, tlsConf *connection.TLSConfig) (string, error) {
	scheme := httpScheme
	if tlsConf != nil && (tlsConf.Mode == connection.OneSideTLSMode || tlsConf.Mode == connection.MutualTLSMode) {
		scheme = httpsScheme
	}
	ret, err := url.JoinPath(scheme, address, metricsSubPath)
	return ret, errors.Wrap(err, "failed to make prometheus URL")
}
