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

	"github.com/hyperledger/fabric-x-common/utils/channel"
	"github.com/hyperledger/fabric-x-common/utils/connection"
	"github.com/hyperledger/fabric-x-common/utils/serve"
	"github.com/hyperledger/fabric-x-common/utils/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type (
	// StartServerParameters defines the parameters for starting servers.
	StartServerParameters struct {
		TLSConfig  connection.TLSConfig
		NumService int
	}

	// Servers holds the server instances and their respective configurations.
	Servers struct {
		Configs     []*serve.Config
		ServersStop []context.CancelFunc
	}
)

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
		sc[i] = test.NewLocalHostServiceConfig(p.TLSConfig)
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
		serverStoppers[i] = test.ServeForTest(ctx, t, c, r)
	}
	return &Servers{
		ServersStop: serverStoppers,
		Configs:     sc,
	}
}

// RunServiceAndServeForTest combines running a service and its GRPC server.
// It is intended for services that implements the Service API (i.e., command line services).
func RunServiceAndServeForTest(
	ctx context.Context,
	tb testing.TB,
	service serve.Service,
	serverConfig ...*serve.Config,
) *channel.Ready {
	tb.Helper()
	doneFlag := RunServiceForTest(ctx, tb, func(ctx context.Context) error {
		return connection.FilterStreamRPCError(service.Run(ctx))
	}, service.WaitForReady)
	for _, c := range serverConfig {
		test.ServeForTest(ctx, tb, c, service)
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
