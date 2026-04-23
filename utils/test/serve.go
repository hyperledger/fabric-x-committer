/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/grpcservice"
)

type (
	// Servers holds the server instances and their respective configurations.
	Servers struct {
		Servers []*grpc.Server
		Configs []*connection.ServerConfig
	}

	// StartServerParameters defines the parameters for starting servers.
	StartServerParameters struct {
		TLSConfig  connection.TLSConfig
		NumService int
	}
)

// RunGrpcServerForTest starts a GRPC server using a register method.
// It handles the cleanup of the GRPC server at the end of a test, and ensure the test is ended
// only when the GRPC server is down.
// It also updates the server config endpoint port to the actual port if the configuration
// did not specify a port.
// The method asserts that the GRPC server did not end with failure.
func RunGrpcServerForTest(
	ctx context.Context, tb testing.TB, serverConfig *connection.ServerConfig, register func(server *grpc.Server),
) *grpc.Server {
	tb.Helper()
	listener, err := serverConfig.Listener(ctx)
	require.NoError(tb, err)
	server, err := serverConfig.GrpcServer(nil)
	require.NoError(tb, err)

	if register != nil {
		register(server)
	} else {
		healthgrpc.RegisterHealthServer(server, connection.DefaultHealthCheckService())
	}

	var wg sync.WaitGroup
	tb.Cleanup(wg.Wait)
	tb.Cleanup(server.Stop)

	// The parent error capture the caller stack trace,
	// which helps track the server origin when debugging test failures.
	parentErr := errors.New("parent stack context")
	wg.Go(func() {
		// We use assert to prevent panicking for cleanup errors.
		serveErr := server.Serve(listener)
		if serveErr != nil && !errors.Is(serveErr, grpc.ErrServerStopped) {
			assert.NoError(tb, errors.WithSecondaryError(serveErr, parentErr))
		}
	})

	_ = context.AfterFunc(ctx, func() {
		server.Stop()
	})
	return server
}

// StartGrpcServersForTest starts multiple GRPC servers with a default configuration.
func StartGrpcServersForTest(
	ctx context.Context,
	t *testing.T,
	p StartServerParameters,
	register func(*grpc.Server),
) *Servers {
	t.Helper()
	sc := make([]*connection.ServerConfig, p.NumService)
	for i := range sc {
		sc[i] = NewLocalHostServer(p.TLSConfig)
	}
	return StartGrpcServersWithConfigForTest(ctx, t, register, sc...)
}

// StartGrpcServersWithConfigForTest starts multiple GRPC servers with given configurations.
func StartGrpcServersWithConfigForTest(
	ctx context.Context, t *testing.T, register func(*grpc.Server), sc ...*connection.ServerConfig,
) *Servers {
	t.Helper()
	grpcServers := make([]*grpc.Server, len(sc))
	if register == nil {
		register = func(server *grpc.Server) {
			healthgrpc.RegisterHealthServer(server, connection.DefaultHealthCheckService())
		}
	}
	for i, s := range sc {
		grpcServers[i] = RunGrpcServerForTest(ctx, t, s, register)
	}
	return &Servers{
		Servers: grpcServers,
		Configs: sc,
	}
}

// RunServiceForTest runs a service using the given service method, and waits for it to be ready
// given the waitFunc method.
// It handles the cleanup of the service at the end of a test, and ensure the test is ended
// only when the service return.
// The method asserts that the service did not end with failure.
// Returns a ready flag that indicate that the service is done.
func RunServiceForTest(
	ctx context.Context,
	tb testing.TB,
	service func(ctx context.Context) error,
	waitFunc func(ctx context.Context) bool,
) *channel.Ready {
	tb.Helper()
	doneFlag := channel.NewReady()
	var wg sync.WaitGroup
	// NOTE: we should cancel the context before waiting for the completion. Therefore, the
	//       order of cleanup matters, which is last added first called.
	tb.Cleanup(wg.Wait)
	dCtx, cancel := context.WithCancel(ctx)
	tb.Cleanup(cancel)

	// We extract caller information to ensure we have sufficient information for debugging.
	pc, file, no, ok := runtime.Caller(1)
	require.True(tb, ok)
	wg.Go(func() {
		defer doneFlag.SignalReady()
		// We use assert to prevent panicking for cleanup errors.
		assert.NoErrorf(tb, service(dCtx), "called from %s:%d\n\t%s", file, no, runtime.FuncForPC(pc).Name())
	})

	if waitFunc == nil {
		return doneFlag
	}

	initCtx, initCancel := context.WithTimeout(dCtx, 2*time.Minute)
	tb.Cleanup(initCancel)
	require.True(tb, waitFunc(initCtx), "service is not ready")
	return doneFlag
}

// RunServiceAndGrpcForTest combines running a service and its GRPC server.
// It is intended for services that implements the Service API (i.e., command line services).
func RunServiceAndGrpcForTest(
	ctx context.Context,
	t *testing.T,
	service grpcservice.Service,
	serverConfig ...*connection.ServerConfig,
) *channel.Ready {
	t.Helper()
	doneFlag := RunServiceForTest(ctx, t, func(ctx context.Context) error {
		return connection.FilterStreamRPCError(service.Run(ctx))
	}, service.WaitForReady)
	for _, server := range serverConfig {
		RunGrpcServerForTest(ctx, t, server, service.RegisterService)
	}
	return doneFlag
}
