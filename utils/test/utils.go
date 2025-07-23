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

	"github.com/onsi/gomega"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/logging"
)

type (
	// GrpcServers holds the server instances and their respective configurations.
	GrpcServers struct {
		Servers []*grpc.Server
		Configs []*connection.ServerConfig
	}
)

// FailHandler registers a [gomega] fail handler.
func FailHandler(t *testing.T) {
	t.Helper()
	gomega.RegisterFailHandler(func(message string, _ ...int) {
		t.Helper()
		t.Errorf("received error message: %s", message)
		t.FailNow()
	})
}

// ServerToClientConfig is used to create client configuration from existing server(s).
func ServerToClientConfig(servers ...*connection.ServerConfig) *connection.ClientConfig {
	endpoints := make([]*connection.Endpoint, len(servers))
	for i, server := range servers {
		endpoints[i] = &server.Endpoint
	}
	return &connection.ClientConfig{
		Endpoints: endpoints,
	}
}

// RunGrpcServerForTest starts a GRPC server using a register method.
// It handles the cleanup of the GRPC server at the end of a test, and ensure the test is ended
// only when the GRPC server is down.
// It also updates the server config endpoint port to the actual port if the configuration
// did not specify a port.
// The method asserts that the GRPC server did not end with failure.
func RunGrpcServerForTest(
	ctx context.Context, t *testing.T, serverConfig *connection.ServerConfig, register func(server *grpc.Server),
) *grpc.Server {
	t.Helper()
	listener, err := serverConfig.Listener()
	require.NoError(t, err)
	server := serverConfig.GrpcServer()

	if register != nil {
		register(server)
	} else {
		healthgrpc.RegisterHealthServer(server, connection.DefaultHealthCheckService())
	}

	var wg sync.WaitGroup
	t.Cleanup(wg.Wait)
	t.Cleanup(server.Stop)
	wg.Add(1)
	go func() {
		defer wg.Done()
		// We use assert to prevent panicking for cleanup errors.
		assert.NoError(t, server.Serve(listener))
	}()

	_ = context.AfterFunc(ctx, func() {
		server.Stop()
	})
	return server
}

// StartGrpcServersForTest starts multiple GRPC servers with a default configuration.
func StartGrpcServersForTest(
	ctx context.Context,
	t *testing.T,
	numService int,
	register func(*grpc.Server, int),
) *GrpcServers {
	t.Helper()
	sc := make([]*connection.ServerConfig, numService)
	for i := range sc {
		sc[i] = connection.NewLocalHostServer()
	}
	return StartGrpcServersWithConfigForTest(ctx, t, sc, register)
}

// StartGrpcServersWithConfigForTest starts multiple GRPC servers with given configurations.
func StartGrpcServersWithConfigForTest(
	ctx context.Context, t *testing.T, sc []*connection.ServerConfig, register func(*grpc.Server, int),
) *GrpcServers {
	t.Helper()
	grpcServers := make([]*grpc.Server, len(sc))
	for i, s := range sc {
		i := i
		grpcServers[i] = RunGrpcServerForTest(ctx, t, s, func(server *grpc.Server) {
			if register != nil {
				register(server, i)
			} else {
				healthgrpc.RegisterHealthServer(server, connection.DefaultHealthCheckService())
			}
		})
	}
	return &GrpcServers{
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
	wg.Add(1)

	// We extract caller information to ensure we have sufficient information for debugging.
	pc, file, no, ok := runtime.Caller(1)
	require.True(tb, ok)
	go func() {
		defer wg.Done()
		defer doneFlag.SignalReady()
		// We use assert to prevent panicking for cleanup errors.
		assert.NoErrorf(tb, service(dCtx), "called from %s:%d\n\t%s", file, no, runtime.FuncForPC(pc).Name())
	}()

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
	service connection.Service,
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

// WaitUntilGrpcServerIsReady uses the health check API to check a service readiness.
func WaitUntilGrpcServerIsReady(
	ctx context.Context,
	t *testing.T,
	conn grpc.ClientConnInterface,
) {
	t.Helper()
	if conn == nil {
		return
	}
	healthClient := healthgrpc.NewHealthClient(conn)
	res, err := healthClient.Check(ctx, &healthgrpc.HealthCheckRequest{}, grpc.WaitForReady(true))
	assert.NotEqual(t, codes.Canceled, status.Code(err))
	require.NoError(t, err)
	require.Equal(t, healthgrpc.HealthCheckResponse_SERVING, res.Status)
}

// StatusRetriever provides implementation retrieve status of given transaction identifiers.
type StatusRetriever interface {
	GetTransactionsStatus(
		context.Context,
		*protoblocktx.QueryStatus,
		...grpc.CallOption,
	) (*protoblocktx.TransactionsStatus, error)
}

// EnsurePersistedTxStatus fails the test if the given TX IDs does not match the expected status.
//
//nolint:revive // maximum number of arguments per function exceeded; max 4 but got 5.
func EnsurePersistedTxStatus(
	ctx context.Context,
	t *testing.T,
	r StatusRetriever,
	txIDs []string,
	expected map[string]*protoblocktx.StatusWithHeight,
) {
	t.Helper()
	if len(txIDs) == 0 {
		return
	}
	actualStatus, err := r.GetTransactionsStatus(ctx, &protoblocktx.QueryStatus{TxIDs: txIDs})
	require.NoError(t, err)
	require.EqualExportedValues(t, expected, actualStatus.Status)
}

// CheckServerStopped returns true if the grpc server listening on a
// given address has been stopped.
func CheckServerStopped(t *testing.T, addr string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	conn, err := grpc.DialContext( //nolint:staticcheck
		ctx,
		addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(), //nolint:staticcheck
	)
	if err != nil {
		return true
	}
	_ = conn.Close()
	return false
}

// SetupDebugging can be added for development to tests that required additional debugging info.
func SetupDebugging() {
	logging.SetupWithConfig(&logging.Config{
		Enabled:     true,
		Level:       logging.Debug,
		Caller:      true,
		Development: true,
	})
}
