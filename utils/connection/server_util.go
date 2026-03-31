/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package connection

import (
	"context"
	"crypto/tls"
	"net"
	"regexp"
	"time"

	"github.com/cenkalti/backoff/v5"
	"github.com/cockroachdb/errors"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"

	"github.com/hyperledger/fabric-x-committer/utils/retry"
)

const tcpProtocol = "tcp"

type (
	// Service describes the methods that are required for a service to run.
	Service interface {
		// Run executes the service until the context is done.
		Run(ctx context.Context) error
		// WaitForReady waits for the service resources to initialize.
		// If the context ended before the service is ready, returns false.
		WaitForReady(ctx context.Context) bool
		// RegisterService registers the supported APIs for this service.
		RegisterService(server *grpc.Server)
		// GetDynamicTLSConfig returns a pre-configured tls.Config for services
		// that support dynamic CA updates.
		// Services without dynamic CAs support return nil.
		// This method is called during TLS handshake (via GetConfigForClient) to retrieve
		// the complete TLS configuration with both static YAML CAs and dynamic config-block CAs.
		// The returned config should be ready to use directly in TLS handshakes.
		GetDynamicTLSConfig(ctx context.Context) *tls.Config
	}
)

var (
	// listenRetry is the acceptable retry profile if port conflicts occur.
	// This handles the race condition where another process claims a pre-assigned port.
	// This will retry with the same port, waiting for it to become available.
	listenRetry = retry.Profile{
		InitialInterval: 50 * time.Millisecond,
		MaxInterval:     500 * time.Millisecond,
		MaxElapsedTime:  2 * time.Minute,
	}

	// portConflictRegex is the compiled regular expression
	// to efficiently detect port binding conflict errors.
	portConflictRegex = regexp.MustCompile(`(?i)(address\s+already\s+in\s+use|port\s+is\s+already\s+allocated)`)
)

// StartService runs a service, waits until it is ready, and register the gRPC server(s).
// It will stop if either the service ended or its respective gRPC server.
func StartService(
	ctx context.Context,
	service Service,
	serverConfigs ...*ServerConfig,
) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	g, gCtx := errgroup.WithContext(ctx)
	g.Go(func() error {
		// If the service stops, there is no reason to continue the GRPC server.
		defer cancel()
		return service.Run(gCtx)
	})

	ctxTimeout, cancelTimeout := context.WithTimeout(gCtx, 5*time.Minute) // TODO: make this configurable.
	defer cancelTimeout()
	if !service.WaitForReady(ctxTimeout) {
		cancel()
		return errors.Wrapf(g.Wait(), "service is not ready")
	}

	for _, server := range serverConfigs {
		g.Go(func() error {
			// If the GRPC servers stop, there is no reason to continue the service.
			defer cancel()
			return RunGrpcServer(gCtx, server, service)
		})
	}
	return g.Wait()
}

// Listener instantiate a [net.Listener] and updates the config port with the effective port.
// If the port is predefined, it will retry to bind to the port until successful or until the context ends.
func (c *ServerConfig) Listener(ctx context.Context) (net.Listener, error) {
	if c.preAllocatedListener != nil {
		return c.preAllocatedListener, nil
	}

	var listener net.Listener
	err := ListenRetryExecute(ctx, func() error {
		var err error
		listener, err = net.Listen(tcpProtocol, c.Endpoint.Address())
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

	logger.Infof("Listening on: %s://%s", tcpProtocol, c.Endpoint.String())
	return listener, nil
}

// ListenRetryExecute executes the provided function with retry logic for port binding conflicts.
// It automatically retries when port conflicts are detected (e.g., "address already in use"),
// using exponential backoff. Non-port-conflict errors are treated as permanent failures
// and will not be retried. The retry behavior is controlled by the listenRetry profile.
func ListenRetryExecute(ctx context.Context, f func() error) error {
	return retry.Execute(ctx, &listenRetry, func() error {
		err := f()
		switch {
		case err == nil:
			return nil
		case portConflictRegex.MatchString(err.Error()):
			// Port conflict - will retry with backoff.
			return errors.Wrap(err, "port conflict")
		default:
			// Not a port conflict - return permanent error to stop retrying.
			return backoff.Permanent(errors.Wrap(err, "creating listener"))
		}
	})
}

// PreAllocateListener is used to allocate a port and bind to ahead of the server initialization.
// It stores the listener object internally to be reused on subsequent calls to Listener().
func (c *ServerConfig) PreAllocateListener(ctx context.Context) (net.Listener, error) {
	listener, err := c.Listener(ctx)
	if err != nil {
		return nil, err
	}
	c.preAllocatedListener = listener
	return listener, nil
}

// ClosePreAllocatedListener closed the pre allocated listener if exists.
func (c *ServerConfig) ClosePreAllocatedListener() error {
	if c.preAllocatedListener == nil {
		return nil
	}
	listener := c.preAllocatedListener
	c.preAllocatedListener = nil
	return listener.Close()
}

// RunGrpcServer runs a server and returns error if failed.
func RunGrpcServer(
	ctx context.Context,
	serverConfig *ServerConfig,
	service Service,
) error {
	listener, err := serverConfig.Listener(ctx)
	if err != nil {
		return err
	}
	//nolint:contextcheck // Context is properly used via chi.Context() in TLS handshake callback
	server, err := serverConfig.GrpcServer(service.GetDynamicTLSConfig)
	if err != nil {
		return errors.Wrapf(err, "failed creating grpc server")
	}
	service.RegisterService(server)

	g, gCtx := errgroup.WithContext(ctx)
	logger.Infof("Serving...")
	g.Go(func() error {
		return server.Serve(listener)
	})
	<-gCtx.Done()
	server.Stop()
	return g.Wait()
}

// RunGrpcServerWithRegister runs a server with a custom register function.
// This is useful for standalone servers that don't implement the full Service interface.
func RunGrpcServerWithRegister(
	ctx context.Context,
	serverConfig *ServerConfig,
	register func(server *grpc.Server),
) error {
	listener, err := serverConfig.Listener(ctx)
	if err != nil {
		return err
	}
	//nolint:contextcheck // Since getDynamicFunc is nil, context will be used.
	server, err := serverConfig.GrpcServer(nil)
	if err != nil {
		return errors.Wrapf(err, "failed creating grpc server")
	}
	register(server)

	g, gCtx := errgroup.WithContext(ctx)
	logger.Infof("Serving...")
	g.Go(func() error {
		return server.Serve(listener)
	})
	<-gCtx.Done()
	server.Stop()
	return g.Wait()
}

// GrpcServer instantiates a gRPC server with the provided configuration.
// If getDynamicFunc is provided, enables dynamic CA certificate support using GetConfigForClient callback.
// Pass nil for getDynamicFunc to use static configuration only.
func (c *ServerConfig) GrpcServer(getDynamicFunc func(ctx context.Context) *tls.Config) (*grpc.Server, error) {
	creds, err := c.TLS.ServerCredentials(getDynamicFunc)
	if err != nil {
		return nil, errors.Wrapf(err, "failed loading the server's grpc credentials")
	}

	opts := []grpc.ServerOption{grpc.MaxRecvMsgSize(maxMsgSize), grpc.MaxSendMsgSize(maxMsgSize)}
	opts = append(opts, grpc.Creds(creds))

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

// DefaultHealthCheckService returns a health-check service that returns SERVING for all services.
func DefaultHealthCheckService() *health.Server {
	healthcheck := health.NewServer()
	healthcheck.SetServingStatus("", healthgrpc.HealthCheckResponse_SERVING)
	return healthcheck
}
