/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package connection

import (
	"context"
	"net"
	"time"

	"github.com/cockroachdb/errors"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
)

const grpcProtocol = "tcp"

type (
	// Service describes the method that are required for a service to run.
	Service interface {
		// Run executes the service until the context is done.
		Run(ctx context.Context) error
		// WaitForReady waits for the service resources to initialize.
		// If the context ended before the service is ready, returns false.
		WaitForReady(ctx context.Context) bool
	}
)

// NewLocalHostServer returns a default server config with endpoint "localhost:0".
func NewLocalHostServer() *ServerConfig {
	return &ServerConfig{Endpoint: *NewLocalHost()}
}

// NewLocalHostServerWithCreds returns a default server config with endpoint "localhost:0" given server credentials.
func NewLocalHostServerWithCreds(creds *ConfigTLS) *ServerConfig {
	return &ServerConfig{
		Endpoint:    *NewLocalHost(),
		ServerCreds: creds,
	}
}

// GrpcServer instantiate a [*grpc.Server].
func (c *ServerConfig) GrpcServer() (*grpc.Server, error) {
	var opts []grpc.ServerOption
	serverGrpcCreds, err := c.ServerCreds.ServerOption()
	if err != nil {
		return nil, errors.Wrapf(err, "failed loading the server's grpc credentials")
	}
	opts = append(opts, serverGrpcCreds)

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
func (c *ServerConfig) Listener() (net.Listener, error) {
	if c.preAllocatedListener != nil {
		return c.preAllocatedListener, nil
	}
	listener, err := net.Listen(grpcProtocol, c.Endpoint.Address())
	if err != nil {
		return nil, errors.Wrap(err, "failed to listen")
	}

	addr := listener.Addr()
	tcpAddress, ok := addr.(*net.TCPAddr)
	if !ok {
		return nil, errors.Join(errors.New("failed to cast to TCP address"), listener.Close())
	}
	c.Endpoint.Port = tcpAddress.Port

	logger.Infof("Listening on: %s://%s", grpcProtocol, c.Endpoint.String())
	return listener, nil
}

// PreAllocateListener is used to allocate a port and bind to ahead of the server initialization.
// It stores the listener object internally to be reused on subsequent calls to Listener().
func (c *ServerConfig) PreAllocateListener() (net.Listener, error) {
	listener, err := c.Listener()
	if err != nil {
		return nil, err
	}
	c.preAllocatedListener = listener
	return listener, nil
}

// RunGrpcServerMainWithError runs a server and returns error if failed.
func RunGrpcServerMainWithError(
	ctx context.Context,
	serverConfig *ServerConfig,
	register func(server *grpc.Server),
) error {
	listener, err := serverConfig.Listener()
	if err != nil {
		return err
	}
	server, err := serverConfig.GrpcServer()
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

// StartService runs a service, waits until it is ready, and register the gRPC server.
// It will stop if either the service ended or its respective gRPC server.
func StartService(
	ctx context.Context,
	service Service,
	serverConfig *ServerConfig,
	register func(server *grpc.Server),
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

	if register != nil && serverConfig != nil {
		g.Go(func() error {
			// If the GRPC server stops, there is no reason to continue the service.
			defer cancel()
			return RunGrpcServerMainWithError(gCtx, serverConfig, func(server *grpc.Server) {
				register(server)
			})
		})
	}
	return g.Wait()
}
