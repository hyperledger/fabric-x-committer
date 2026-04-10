/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package monitoring

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/pprof"
	"net/url"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-lib-go/common/flogging"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/sync/errgroup"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

// HTTP logger is defined in provider.go

const (
	httpsScheme    = "https://"
	httpScheme     = "http://"
	metricsSubPath = "/metrics"
	pprofSubPath   = "/debug/pprof/"
)

var httpLogger = flogging.MustGetLogger("monitoring.http")

// httpServer wraps a http.Server to export metrics and pprof endpoints.
type httpServer struct {
	mux      *http.ServeMux
	config   *connection.ServerConfig
	registry *prometheus.Registry
}

// StartHTTPServer creates and starts an HTTP monitoring server asynchronously.
// It synchronously creates the listener (returning any immediate errors like port
// conflicts), then starts the server in a goroutine and returns.
// Errors during serving (after successful listener creation) are logged via httpLogger.
func StartHTTPServer(
	ctx context.Context,
	config *connection.ServerConfig,
	registry *prometheus.Registry,
) error {
	srv := newHTTPServer(config, registry)
	listener, err := srv.createListener(ctx)
	if err != nil {
		return err
	}
	go srv.serve(ctx, listener)
	return nil
}

// newHTTPServer creates a new HTTP monitoring server but does not start it.
// This is primarily intended for testing; production code should use StartHTTPServer.
func newHTTPServer(config *connection.ServerConfig, registry *prometheus.Registry) *httpServer {
	mux := http.NewServeMux()

	// Register prometheus handler
	mux.Handle(
		metricsSubPath,
		promhttp.HandlerFor(
			registry,
			promhttp.HandlerOpts{
				Registry: registry,
			},
		),
	)

	// Register pprof handlers
	mux.HandleFunc(pprofSubPath, pprof.Index)
	mux.HandleFunc(pprofSubPath+"cmdline", pprof.Cmdline)
	mux.HandleFunc(pprofSubPath+"profile", pprof.Profile)
	mux.HandleFunc(pprofSubPath+"symbol", pprof.Symbol)
	mux.HandleFunc(pprofSubPath+"trace", pprof.Trace)

	return &httpServer{
		mux:      mux,
		config:   config,
		registry: registry,
	}
}

// createListener creates the listener and performs synchronous setup.
// Returns the configured listener or an error if setup fails.
// This allows StartHTTPServer to return early on configuration errors.
func (s *httpServer) createListener(ctx context.Context) (net.Listener, error) {
	httpLogger.Debugf("Creating listener for HTTP monitoring server with secure mode: %v", s.config.TLS.Mode)

	// Generate TLS configuration
	serverTLSConfig, err := s.config.TLS.ServerTLSConfig()
	if err != nil {
		return nil, errors.Wrap(err, "failed to create TLS credentials for HTTP monitoring server")
	}

	// Create listener from config with retry for port conflicts
	listener, err := s.config.Listener(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create listener for HTTP monitoring server")
	}

	if serverTLSConfig != nil {
		listener = tls.NewListener(listener, serverTLSConfig)
	}

	// Build URL for logging
	metricsURL, err := MakeMetricsURL(listener.Addr().String(), serverTLSConfig)
	if err != nil {
		connection.CloseConnectionsLog(listener)
		return nil, errors.Wrap(err, "failed to create metrics URL")
	}
	httpLogger.Infof("HTTP monitoring server ready to serve metrics at: %s", metricsURL)
	return listener, nil
}

// serve starts the HTTP monitoring server with context awareness.
// It runs in a goroutine and handles graceful shutdown when the context is cancelled.
// Errors during serving are logged via httpLogger.
func (s *httpServer) serve(parentCtx context.Context, listener net.Listener) {
	server := &http.Server{
		ReadTimeout: 30 * time.Second,
		Handler:     s.mux,
	}

	// Use errgroup for server goroutine - child context signals completion
	g := new(errgroup.Group)
	childCtx, childCancel := context.WithCancel(parentCtx)

	g.Go(func() error {
		defer childCancel() // Signal completion (success or error)
		err := server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	})

	// Wait for parent context cancellation or server completion
	select {
	case <-parentCtx.Done():
		// Parent cancelled - initiate graceful shutdown
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer shutdownCancel()
		//nolint:contextcheck // parentCtx is already done; need fresh context for graceful shutdown
		if err := server.Shutdown(shutdownCtx); err != nil {
			httpLogger.Warnf("HTTP monitoring server graceful shutdown error: %v", err)
		}
		// Wait for goroutine to finish after listener closes
		_ = g.Wait()
	case <-childCtx.Done():
		// Server exited (either errored or context was cancelled)
		if err := g.Wait(); err != nil {
			httpLogger.Errorf("HTTP monitoring server stopped with error: %v", err)
			return
		}
		httpLogger.Info("HTTP monitoring server stopped")
	}
}

// MakeMetricsURL constructs the Prometheus metrics URL.
// Based on the secure level, we set the url scheme to http or https.
func MakeMetricsURL(address string, tlsConf *tls.Config) (string, error) {
	scheme := httpScheme
	if tlsConf != nil {
		scheme = httpsScheme
	}
	ret, err := url.JoinPath(scheme, address, metricsSubPath)
	return ret, errors.Wrap(err, "failed to make prometheus URL")
}
