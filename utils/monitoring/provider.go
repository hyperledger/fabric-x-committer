/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package monitoring

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/sync/errgroup"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/logging"
)

const (
	httpsScheme    = "https://"
	httpScheme     = "http://"
	metricsSubPath = "/metrics"
)

// Provider is a prometheus metrics provider.
type Provider struct {
	registry *prometheus.Registry
	url      string
}

var logger = logging.New("monitoring")

// NewProvider creates a new prometheus metrics provider.
func NewProvider() *Provider {
	return &Provider{registry: prometheus.NewRegistry()}
}

// StartPrometheusServer starts a prometheus server.
// It also starts the given monitoring methods. Their context will cancel once the server is cancelled.
// This method returns once the server is shutdown and all monitoring methods returns.
func (p *Provider) StartPrometheusServer(
	ctx context.Context, serverConfig *connection.ServerConfig, monitor ...func(context.Context),
) error {
	logger.Debugf("Creating prometheus server with TLS mode: %v", serverConfig.TLS.Mode)
	var securedMetrics bool
	// Generate TLS configuration from the server config.
	serverTLSConfig, err := connection.NewServerTLSConfig(serverConfig.TLS)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle(
		metricsSubPath,
		promhttp.HandlerFor(
			p.Registry(),
			promhttp.HandlerOpts{
				Registry: p.Registry(),
			},
		),
	)
	server := &http.Server{
		ReadTimeout: 30 * time.Second,
		Handler:     mux,
		TLSConfig:   serverTLSConfig,
	}

	l, err := serverConfig.Listener(ctx)
	if err != nil {
		return err
	}
	defer connection.CloseConnectionsLog(l)

	p.url, securedMetrics, err = MakeMetricsURL(l.Addr().String(), serverConfig.TLS.Mode)
	if err != nil {
		return err
	}

	g, gCtx := errgroup.WithContext(ctx)
	g.Go(func() error {
		logger.Infof("Prometheus serving on URL: %s", p.url)
		defer logger.Info("Prometheus stopped serving")
		if securedMetrics {
			return server.ServeTLS(l, serverConfig.TLS.CertPath, serverConfig.TLS.KeyPath)
		}
		return server.Serve(l)
	})

	// The following ensures the method does not return before all monitor methods return.
	for _, m := range monitor {
		g.Go(func() error {
			m(gCtx)
			return nil
		})
	}

	// The following ensures the method does not return before the close procedure is complete.
	stopAfter := context.AfterFunc(ctx, func() {
		g.Go(func() error {
			if errClose := server.Close(); err != nil {
				return errors.Wrap(errClose, "failed to close prometheus server")
			}
			return nil
		})
	})
	defer stopAfter()

	if err = g.Wait(); !errors.Is(err, http.ErrServerClosed) {
		return errors.Wrap(err, "prometheus server stopped with an error")
	}
	return nil
}

// URL returns the prometheus server URL.
func (p *Provider) URL() string {
	return p.url
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
func MakeMetricsURL(address, tlsMode string) (string, bool, error) {
	var (
		securedMetrics bool
		scheme         string
	)
	logger.Infof("tls-mode-metricsURL: %v", tlsMode)
	switch tlsMode {
	case connection.UnmentionedTLSMode, connection.NoneTLSMode:
		scheme = httpScheme
	case connection.OneSideTLSMode, connection.MutualTLSMode:
		scheme = httpsScheme
		securedMetrics = true
	default:
		return "", false, errors.Newf("unknown TLS mode: %s", tlsMode)
	}
	ret, err := url.JoinPath(scheme, address, metricsSubPath)
	return ret, securedMetrics, errors.Wrap(err, "failed to make prometheus URL")
}
