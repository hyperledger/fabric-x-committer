/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package connection

import (
	"context"
	"net"
	"regexp"
	"time"

	"github.com/cenkalti/backoff/v5"
	"github.com/cockroachdb/errors"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"

	"github.com/hyperledger/fabric-x-committer/utils/retry"
)

const tcpProtocol = "tcp"

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

// GrpcServer instantiate a [grpc.Server].
func (c *ServerConfig) GrpcServer() (*grpc.Server, error) {
	opts := []grpc.ServerOption{grpc.MaxRecvMsgSize(maxMsgSize), grpc.MaxSendMsgSize(maxMsgSize)}
	serverGrpcTransportCreds, err := c.TLS.ServerCredentials()
	if err != nil {
		return nil, errors.Wrapf(err, "failed loading the server's grpc credentials")
	}
	opts = append(opts, grpc.Creds(serverGrpcTransportCreds))

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

// DefaultHealthCheckService returns a health-check service that returns SERVING for all services.
func DefaultHealthCheckService() *health.Server {
	healthcheck := health.NewServer()
	healthcheck.SetServingStatus("", healthgrpc.HealthCheckResponse_SERVING)
	return healthcheck
}
