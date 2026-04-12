/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package servicelifecycle_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/servicelifecycle"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

// stubService implements servicelifecycle.Service with a health check server.
type stubService struct {
	ready   *channel.Ready
	running *channel.Ready
}

func newStubService() *stubService {
	return &stubService{
		ready:   channel.NewReady(),
		running: channel.NewReady(),
	}
}

func (s *stubService) Run(ctx context.Context) error {
	s.running.SignalReady()
	<-ctx.Done()
	return nil
}

func (s *stubService) WaitForReady(_ context.Context) bool {
	s.ready.SignalReady()
	return true
}

func (*stubService) RegisterService(server *grpc.Server) {
	healthgrpc.RegisterHealthServer(server, connection.DefaultHealthCheckService())
}

func (*stubService) StartMonitoringServer(_ context.Context) error {
	return nil
}

// slowReadyService blocks WaitForReady until context expires.
type slowReadyService struct{}

func (*slowReadyService) Run(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

func (*slowReadyService) WaitForReady(ctx context.Context) bool {
	<-ctx.Done()
	return false
}

func (*slowReadyService) RegisterService(server *grpc.Server) {
	healthgrpc.RegisterHealthServer(server, connection.DefaultHealthCheckService())
}

func (*slowReadyService) StartMonitoringServer(_ context.Context) error {
	return nil
}

func TestStartAndServe(t *testing.T) {
	t.Parallel()

	t.Run("starts service and serves gRPC", func(t *testing.T) {
		t.Parallel()
		serverConfig := test.NewLocalHostServer(test.InsecureTLSConfig)
		svc := newStubService()

		startInBackground(t, svc, serverConfig)

		waitCtx, waitCancel := context.WithTimeout(t.Context(), 10*time.Second)
		t.Cleanup(waitCancel)
		require.True(t, svc.running.WaitForReady(waitCtx), "service did not start running")
		require.True(t, svc.ready.WaitForReady(waitCtx), "WaitForReady was not called")
		requireHealthy(t, serverConfig)
	})

	t.Run("stops when service is not ready", func(t *testing.T) {
		t.Parallel()
		serverConfig := test.NewLocalHostServer(test.InsecureTLSConfig)

		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		t.Cleanup(cancel)

		err := servicelifecycle.StartAndServe(ctx, &slowReadyService{}, serverConfig)
		require.NoError(t, err)
		assert.Equal(t, 0, serverConfig.Endpoint.Port, "server should not have started")
	})

	t.Run("serves on multiple server configs", func(t *testing.T) {
		t.Parallel()
		serverConfig1 := test.NewLocalHostServer(test.InsecureTLSConfig)
		serverConfig2 := test.NewLocalHostServer(test.InsecureTLSConfig)

		startInBackground(t, newStubService(), serverConfig1, serverConfig2)

		requireHealthy(t, serverConfig1)
		requireHealthy(t, serverConfig2)
	})
}

func requireHealthy(t *testing.T, sc *connection.ServerConfig) {
	t.Helper()

	require.NotZero(t, sc.Endpoint.Port, "server did not bind to a port")
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		assert.NoError(ct, connection.RunHealthCheck(t.Context(), sc.Endpoint, sc.TLS))
	}, 5*time.Second, 50*time.Millisecond, "health check did not pass")
}

// startInBackground calls StartAndServe in a background goroutine.
// Pre-allocates listeners so Endpoint.Port is set before any goroutine starts,
// avoiding a data race between Listener()'s port assignment and test reads.
func startInBackground(
	t *testing.T, service servicelifecycle.Service, serverConfigs ...*connection.ServerConfig,
) {
	t.Helper()

	for _, sc := range serverConfigs {
		_, err := sc.PreAllocateListener(t.Context())
		require.NoError(t, err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	g, gCtx := errgroup.WithContext(ctx)
	g.Go(func() error {
		return servicelifecycle.StartAndServe(gCtx, service, serverConfigs...)
	})

	t.Cleanup(func() { cancel(); assert.NoError(t, g.Wait()) })
}
