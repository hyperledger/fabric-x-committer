/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package grpcservice

import (
	"context"
	"crypto/tls"
	"net"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-lib-go/common/flogging"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

var logger = flogging.MustGetLogger("grpcservice")

// Service is for full lifecycle services that run, signal readiness,
// and register on a gRPC server.
type Service interface {
	Registerer
	TLSConfigRefresher
	// Run executes the service until the context is done.
	Run(ctx context.Context) error
	// WaitForReady waits for the service resources to initialize.
	// If the context ended before the service is ready, returns false.
	WaitForReady(ctx context.Context) bool
}

// Registerer is for services that register on a gRPC server.
type Registerer interface {
	RegisterService(server *grpc.Server)
}

// TLSConfigRefresher is for services that provide dynamic CA certificate support for TLS handshakes.
type TLSConfigRefresher interface {
	// GetTLSConfig returns a pre-configured tls.Config for services
	// that support dynamic CA updates.
	// Services without dynamic CAs support return nil.
	// This method is called during TLS handshake (via GetConfigForClient) to retrieve
	// the complete TLS configuration with both static YAML CAs and dynamic config-block CAs.
	// The returned config should be ready to use directly in TLS handshakes.
	GetTLSConfig(ctx context.Context) *tls.Config
}

// StartAndServe runs a full lifecycle service: starts the service, waits for it
// to be ready, then creates and serves gRPC server(s). Stops everything
// if either the service or any server exits.
func StartAndServe(ctx context.Context, service Service, serverConfigs ...*connection.ServerConfig) error {
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

	for _, sc := range serverConfigs {
		g.Go(func() error {
			// If the GRPC servers stop, there is no reason to continue the service.
			defer cancel()
			return Serve(gCtx, service, sc)
		})
	}
	return g.Wait()
}

// Serve creates a gRPC server and listener from the config, registers the
// service, and serves until the context is done. This is for services that implement the full Service interface.
func Serve(ctx context.Context, service Service, serverConfig *connection.ServerConfig) error {
	listener, err := serverConfig.Listener(ctx)
	if err != nil {
		return err
	}

	server, err := serverConfig.GrpcServer(service.GetTLSConfig)
	if err != nil {
		return errors.Wrapf(err, "failed creating grpc server")
	}
	service.RegisterService(server)
	return runServer(ctx, server, listener)
}

// MockServe creates a gRPC server and listener from the config, registers the
// service, and serves until the context is done. For services that only
// implement Registerer (e.g., mock services without Run/WaitForReady).
func MockServe(ctx context.Context, service Registerer, serverConfig *connection.ServerConfig) error {
	listener, err := serverConfig.Listener(ctx)
	if err != nil {
		return err
	}
	server, err := serverConfig.GrpcServer(nil)
	if err != nil {
		return errors.Wrapf(err, "failed creating grpc server")
	}
	service.RegisterService(server)
	return runServer(ctx, server, listener)
}

// runServer handles the blocking execution and graceful shutdown of a configured gRPC server.
func runServer(ctx context.Context, server *grpc.Server, listener net.Listener) error {
	logger.Infof("Serving...")
	g, gCtx := errgroup.WithContext(ctx)
	g.Go(func() error {
		return server.Serve(listener)
	})
	<-gCtx.Done()
	server.Stop()
	return g.Wait()
}
