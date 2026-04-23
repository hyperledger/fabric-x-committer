/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package serve

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/hyperledger/fabric-lib-go/common/flogging"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

type (
	// Service is for full lifecycle services that run, signal readiness,
	// and register on gRPC and HTTP servers.
	Service interface {
		Registerer
		// Run executes the service until the context is done.
		Run(ctx context.Context) error
		// WaitForReady waits for the service resources to initialize.
		// If the context ended before the service is ready, returns false.
		WaitForReady(ctx context.Context) bool
	}

	// Registerer is for services that register on gRPC and HTTP servers.
	// It is used to register the service gRPC, HTTP, and monitoring servers.
	Registerer interface {
		RegisterService(Servers)
	}

	// Servers holds the gRPC, HTTP, and monitoring servers along with their listeners.
	// It provides a unified interface for service registration and lifecycle management.
	Servers struct {
		GRPC            *grpc.Server
		HTTP            *runtime.ServeMux
		Monitoring      *http.ServeMux
		GrpcTLSProvider *TLSProvider

		httpServer       *http.Server
		monitoringServer *http.Server

		grpcListener       net.Listener
		httpListener       net.Listener
		monitoringListener net.Listener

		// Use sync.Once to ensure the Stop/Close methods are only called once, even if triggered
		// from multiple goroutines (AfterFunc callback and main flow).
		stopOnce *sync.Once
	}
)

var logger = flogging.MustGetLogger("serve")

// StartAndServe runs a full lifecycle service: starts the service, waits for it
// to be ready, then creates and serves gRPC and HTTP/monitoring server(s).
func StartAndServe(ctx context.Context, service Service, serverConfig ...*Config) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	g, gCtx := errgroup.WithContext(ctx)
	g.Go(func() error {
		// If the service stops, there is no reason to continue the servers.
		defer cancel()
		return service.Run(gCtx)
	})

	ctxTimeout, cancelTimeout := context.WithTimeout(gCtx, 5*time.Minute) // TODO: make this configurable.
	defer cancelTimeout()
	if !service.WaitForReady(ctxTimeout) {
		cancel()
		return errors.Wrapf(g.Wait(), "service is not ready")
	}

	// Start servers.
	for _, sc := range serverConfig {
		g.Go(func() error {
			// If one of the servers stop, there is no reason to continue the service.
			defer cancel()
			return Serve(gCtx, service, sc)
		})
	}

	return g.Wait()
}

// Serve creates servers from the config, registers the service, and serves until the context is done.
// It starts the main gRPC server and optionally an HTTP/monitoring server if configured.
func Serve(ctx context.Context, r Registerer, conf *Config) error {
	if conf == nil {
		return nil
	}

	servers, err := NewServers(ctx, conf)
	defer servers.Stop()
	if err != nil {
		return err
	}
	return servers.Serve(ctx, r)
}

// NewServers creates and initializes server instances from the provided configuration.
// It sets up gRPC, HTTP, and monitoring servers along with their listeners.
// IMPORTANT: If error is returned, the caller is responsible for calling StopFunc() on the
// returned Servers.
func NewServers(ctx context.Context, conf *Config) (s Servers, err error) {
	s.stopOnce = &sync.Once{}

	s.GrpcTLSProvider, err = NewTLSProvider(conf.GRPC.TLS)
	if err != nil {
		return s, errors.Wrap(err, "failed to create TLS provider")
	}

	s.GRPC, err = newGRPCServer(&conf.GRPC, s.GrpcTLSProvider)
	if err != nil {
		return s, errors.Wrapf(err, "failed creating GRPC server")
	}

	s.HTTP = runtime.NewServeMux()
	s.httpServer, err = newHTTPServer(&conf.HTTP, s.HTTP)
	if err != nil {
		return s, errors.Wrapf(err, "failed creating HTTP server")
	}

	s.Monitoring = http.NewServeMux()
	s.monitoringServer, err = newHTTPServer(&conf.Monitoring, s.Monitoring)
	if err != nil {
		return s, errors.Wrapf(err, "failed creating monitoring server")
	}

	if !conf.GRPC.Endpoint.Empty() {
		s.grpcListener, err = conf.GRPC.Listener(ctx)
		if err != nil {
			return s, err
		}
	}
	if !conf.HTTP.Endpoint.Empty() {
		s.httpListener, err = newHTTPListener(ctx, &conf.HTTP, s.httpServer.TLSConfig)
		if err != nil {
			return s, err
		}
		s.httpServer.Addr = s.httpListener.Addr().String()
	}
	if !conf.Monitoring.Endpoint.Empty() {
		s.monitoringListener, err = newHTTPListener(ctx, &conf.Monitoring, s.monitoringServer.TLSConfig)
		if err != nil {
			return s, err
		}
		s.monitoringServer.Addr = s.monitoringListener.Addr().String()
	}

	return s, nil
}

// Serve registers the service and starts serving on all configured endpoints.
// It blocks until the context is canceled or an error occurs.
func (s *Servers) Serve(ctx context.Context, r Registerer) error {
	defer s.Stop()

	// The following ensures Stop() is called when the context is canceled.
	stopAfter := context.AfterFunc(ctx, s.Stop)
	defer stopAfter()

	r.RegisterService(*s)

	g, gCtx := errgroup.WithContext(ctx)
	if s.grpcListener != nil {
		g.Go(func() error {
			logger.Infof("Serving gRPC on %s...", s.grpcListener.Addr())
			serveErr := s.GRPC.Serve(s.grpcListener)
			logger.Infof("Stopped serving gRPC on %s...", s.grpcListener.Addr())
			return serveErr
		})
	}
	if s.httpListener != nil {
		g.Go(func() error {
			logger.Infof("Serving HTTP on %s...", s.httpListener.Addr())
			serveErr := s.httpServer.Serve(s.httpListener)
			logger.Infof("Stopped serving HTTP on %s...", s.httpListener.Addr())
			return serveErr
		})
	}
	if s.monitoringListener != nil {
		g.Go(func() error {
			logger.Infof("Serving Monitoring on %s...", s.monitoringListener.Addr())
			serveErr := s.monitoringServer.Serve(s.monitoringListener)
			logger.Infof("Stopped serving Monitoring on %s...", s.monitoringListener.Addr())
			return serveErr
		})
	}

	// Handle gRPC server shutdown.
	<-gCtx.Done()
	s.Stop()
	err := g.Wait()
	if err == nil || errors.Is(err, http.ErrServerClosed) || errors.Is(err, grpc.ErrServerStopped) {
		return nil
	}
	return err
}

// Stop stops the servers.
func (s *Servers) Stop() {
	s.stopOnce.Do(func() {
		if s.GRPC != nil {
			s.GRPC.Stop()
		}
		if s.httpServer != nil {
			_ = s.httpServer.Close()
		}
		if s.monitoringServer != nil {
			_ = s.monitoringServer.Close()
		}
		connection.CloseConnectionsLog(s.grpcListener, s.httpListener, s.monitoringListener)
	})
}

// Listener instantiates a [net.Listener] and updates the config port with the effective port.
// If the port is predefined, it retries to bind to the port until successful or until the context ends.
func (c *ServerConfig) Listener(ctx context.Context) (net.Listener, error) {
	if c.preAllocatedListener != nil {
		return c.preAllocatedListener, nil
	}

	var listener net.Listener
	err := ListenRetryExecute(ctx, func() error {
		var err error
		listener, err = net.Listen("tcp", c.Endpoint.Address())
		return err
	})
	if err != nil {
		return nil, errors.Wrap(err, "failed to listen")
	}

	addr := listener.Addr()
	tcpAddress, ok := addr.(*net.TCPAddr)
	if !ok {
		return nil, errors.Join(errors.New("failed to cast to TCP address"), listener.Close())
	}
	c.Endpoint.Port = tcpAddress.Port

	logger.Infof("Listening on: tcp://%s", c.Endpoint.String())
	return listener, nil
}

func newHTTPListener(ctx context.Context, c *ServerConfig, tlsConfig *tls.Config) (net.Listener, error) {
	l, err := c.Listener(ctx)
	if err != nil {
		return nil, err
	}
	if tlsConfig != nil {
		l = tls.NewListener(l, tlsConfig)
	}
	return l, nil
}

// newGRPCServer instantiate a [grpc.Server].
func newGRPCServer(c *ServerConfig, tlsProvider *TLSProvider) (*grpc.Server, error) {
	opts := []grpc.ServerOption{
		grpc.MaxRecvMsgSize(connection.MaxMsgSize),
		grpc.MaxSendMsgSize(connection.MaxMsgSize),
	}
	opts = append(opts, grpc.Creds(newCredentials(tlsProvider.GetServerTLSCredentials())))

	if err := c.RateLimit.Validate(); err != nil {
		return nil, errors.Wrap(err, "invalid rate limit configuration")
	}

	if limiter := NewRateLimiter(&c.RateLimit); limiter != nil {
		opts = append(opts, grpc.UnaryInterceptor(RateLimitInterceptor(limiter)))
		logger.Infof("Rate limiting enabled: %d requests/second, burst: %d",
			c.RateLimit.RequestsPerSecond, c.RateLimit.Burst)
	}

	if sem := NewConcurrencyLimit(c.MaxConcurrentStreams); sem != nil {
		opts = append(opts, grpc.StreamInterceptor(StreamConcurrencyInterceptor(sem)))
		logger.Infof("Stream concurrency limit enabled: %d max concurrent streams", c.MaxConcurrentStreams)
	}

	if c.KeepAlive != nil && c.KeepAlive.Params != nil {
		opts = append(opts, grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionIdle:     c.KeepAlive.Params.MaxConnectionIdle,
			MaxConnectionAge:      c.KeepAlive.Params.MaxConnectionAge,
			MaxConnectionAgeGrace: c.KeepAlive.Params.MaxConnectionAgeGrace,
			Time:                  c.KeepAlive.Params.Time,
			Timeout:               c.KeepAlive.Params.Timeout,
		}))
	}
	if c.KeepAlive != nil && c.KeepAlive.EnforcementPolicy != nil {
		opts = append(opts, grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             c.KeepAlive.EnforcementPolicy.MinTime,
			PermitWithoutStream: c.KeepAlive.EnforcementPolicy.PermitWithoutStream,
		}))
	}
	return grpc.NewServer(opts...), nil
}

func newCredentials(tlsCfg *tls.Config) credentials.TransportCredentials {
	if tlsCfg == nil {
		return insecure.NewCredentials()
	}
	return credentials.NewTLS(tlsCfg)
}

// newHTTPServer instantiate a [http.Server].
func newHTTPServer(c *ServerConfig, handler http.Handler) (*http.Server, error) {
	serverCreds, err := connection.NewServerTLSCredentials(c.TLS)
	if err != nil {
		return nil, err
	}
	tlsConfig, err := serverCreds.CreateServerTLSConfig()
	if err != nil {
		return nil, err
	}
	return &http.Server{
		TLSConfig:   tlsConfig,
		ReadTimeout: time.Minute,
		Handler:     handler,
	}, nil
}
