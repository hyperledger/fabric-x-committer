/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package grpcservice

import (
	"context"
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
	// Check if the service supports dynamic TLS using type assertion
	var dynamicService connection.DynamicTLSService
	if ds, ok := service.(connection.DynamicTLSService); ok {
		dynamicService = ds
	}

	//nolint:contextcheck // Context from chi.Context() is passed to GetTLSConfig during TLS handshake.
	server, err := serverConfig.GrpcServer(dynamicService)
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
	//nolint:contextcheck // Mock services don't support dynamic TLS, so nil is passed.
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
