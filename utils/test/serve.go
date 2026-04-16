/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"context"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-x-common/tools/cryptogen"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/retry"
	"github.com/hyperledger/fabric-x-committer/utils/serve"
)

var (
	// InsecureTLSConfig defines an empty tls config.
	InsecureTLSConfig connection.TLSConfig
	// defaultGrpcRetryProfile defines the retry policy for a gRPC client connection.
	defaultGrpcRetryProfile retry.Profile

	// OrgRootCA is the path to organization 0's TLS client credentials in the crypto materials directory.
	OrgRootCA = filepath.Join(cryptogen.PeerOrganizationsDir, "peer-org-0",
		cryptogen.MSPDir, cryptogen.TLSCaCertsDir, "tlspeer-org-0-CA-cert.pem")

	// OrdererRootCATLSPath is the path to organization 0's orderer TLS credentials in the crypto materials directory.
	OrdererRootCATLSPath = filepath.Join(cryptogen.OrdererOrganizationsDir,
		"orderer-org-0", cryptogen.MSPDir, cryptogen.TLSCaCertsDir, "tlsorderer-org-0-CA-cert.pem")
)

type (
	// Servers holds the server instances and their respective configurations.
	Servers struct {
		Configs     []*serve.Config
		ServersStop []context.CancelFunc
	}

	// StartServerParameters defines the parameters for starting servers.
	StartServerParameters struct {
		TLSConfig  connection.TLSConfig
		NumService int
	}

	// HealthService is a test helper that implements grpcservice.Registerer
	// and provides a default health check service for gRPC servers in tests.
	HealthService struct {
		healthgrpc.HealthServer
	}
)

// ServeForTest starts a GRPC server and optionally a monitoring server using a register method.
// It handles the cleanup of the servers at the end of a test, and ensure the test is ended
// only when the servers are down.
// It also updates the server config endpoint port to the actual port if the configuration
// did not specify a port.
// The method asserts that the servers did not end with failure.
func ServeForTest(
	ctx context.Context, tb testing.TB, sc *serve.Config, registerer serve.Registerer,
) (stop context.CancelFunc) {
	tb.Helper()

	servers, err := serve.NewServers(ctx, sc)
	tb.Cleanup(servers.Stop)
	require.NoError(tb, err)

	if registerer == nil {
		registerer = &HealthService{HealthServer: serve.DefaultHealthCheckService()}
	}

	var wg sync.WaitGroup
	tb.Cleanup(wg.Wait)

	// The parent error capture the caller stack trace,
	// which helps track the server origin when debugging test failures.
	parentErr := errors.New("parent stack context")
	wg.Go(func() {
		serveErr := servers.Serve(ctx, registerer)
		// We use assert to prevent panicking for cleanup errors.
		if serveErr != nil {
			assert.NoError(tb, errors.WithSecondaryError(serveErr, parentErr))
		}
	})
	return servers.Stop
}

// ServeManyForTest starts multiple GRPC servers with a default configuration.
func ServeManyForTest(
	ctx context.Context,
	t *testing.T,
	p StartServerParameters,
	r serve.Registerer,
) *Servers {
	t.Helper()
	sc := make([]*serve.Config, p.NumService)
	for i := range sc {
		sc[i] = NewLocalHostServiceConfig(p.TLSConfig)
	}
	return ServeManyWithConfigForTest(ctx, t, r, sc...)
}

// ServeManyWithConfigForTest starts multiple GRPC servers with given configurations.
func ServeManyWithConfigForTest(
	ctx context.Context, t *testing.T, r serve.Registerer, sc ...*serve.Config,
) *Servers {
	t.Helper()
	serverStoppers := make([]context.CancelFunc, len(sc))
	for i, c := range sc {
		serverStoppers[i] = ServeForTest(ctx, t, c, r)
	}
	return &Servers{
		ServersStop: serverStoppers,
		Configs:     sc,
	}
}

// RegisterService registers the health check service with the gRPC server.
// This implements the grpcservice.Registerer interface.
func (h *HealthService) RegisterService(s serve.Servers) {
	healthgrpc.RegisterHealthServer(s.GRPC, h)
}

// RunServiceAndServeForTest combines running a service and its GRPC server.
// It is intended for services that implements the Service API (i.e., command line services).
func RunServiceAndServeForTest(
	ctx context.Context,
	t *testing.T,
	service serve.Service,
	serverConfig ...*serve.Config,
) *channel.Ready {
	t.Helper()
	doneFlag := RunServiceForTest(ctx, t, func(ctx context.Context) error {
		return connection.FilterStreamRPCError(service.Run(ctx))
	}, service.WaitForReady)
	for _, c := range serverConfig {
		ServeForTest(ctx, t, c, service)
	}
	return doneFlag
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
