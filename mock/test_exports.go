/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package mock

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	commontypes "github.com/hyperledger/fabric-x-common/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

// StartMockVerifierService starts a specified number of mock verifier service and register cancellation.
func StartMockVerifierService(t *testing.T, p test.StartServerParameters) (
	*Verifier, *test.GrpcServers,
) {
	t.Helper()
	mockVerifier := NewMockSigVerifier()
	verifierGrpc := test.StartGrpcServersForTest(t.Context(), t, p, mockVerifier.RegisterService)
	return mockVerifier, verifierGrpc
}

// StartMockVerifierServiceFromServerConfig starts a specified number of mock verifier service.
func StartMockVerifierServiceFromServerConfig(
	t *testing.T, verifier *Verifier, sc ...*connection.ServerConfig,
) *test.GrpcServers {
	t.Helper()
	return test.StartGrpcServersWithConfigForTest(t.Context(), t, verifier.RegisterService, sc...)
}

// StartMockVCService starts a specified number of mock VC service using the same shared instance.
// It is used for testing when multiple VC services are required to share the same state.
func StartMockVCService(t *testing.T, p test.StartServerParameters) (*VcService, *test.GrpcServers) {
	t.Helper()
	sharedVC := NewMockVcService()
	vcGrpc := test.StartGrpcServersForTest(t.Context(), t, p, sharedVC.RegisterService)
	return sharedVC, vcGrpc
}

// StartMockVCServiceFromServerConfig starts a specified number of mock vc service.
func StartMockVCServiceFromServerConfig(
	t *testing.T, vc *VcService, sc ...*connection.ServerConfig,
) *test.GrpcServers {
	t.Helper()
	return test.StartGrpcServersWithConfigForTest(t.Context(), t, vc.RegisterService, sc...)
}

// StartMockCoordinatorService starts a mock coordinator service and registers cancellation.
func StartMockCoordinatorService(t *testing.T, p test.StartServerParameters) (
	*Coordinator, *test.GrpcServers,
) {
	t.Helper()
	p.NumService = 1
	mockCoordinator := NewMockCoordinator()
	coordinatorGrpc := test.StartGrpcServersForTest(
		t.Context(), t, p, mockCoordinator.RegisterService,
	)
	return mockCoordinator, coordinatorGrpc
}

// StartMockCoordinatorServiceFromServerConfig starts a mock coordinator service using the given config.
func StartMockCoordinatorServiceFromServerConfig(
	t *testing.T,
	coordService *Coordinator,
	sc *connection.ServerConfig,
) *test.GrpcServers {
	t.Helper()
	return test.StartGrpcServersWithConfigForTest(t.Context(), t, coordService.RegisterService, sc)
}

// StartMockOrderingServices starts a specified number of mock ordering service and register cancellation.
func StartMockOrderingServices(t *testing.T, conf *OrdererConfig) (
	*Orderer, *test.GrpcServers,
) {
	t.Helper()
	service, err := NewMockOrderer(conf)
	require.NoError(t, err)
	test.RunServiceForTest(t.Context(), t, func(ctx context.Context) error {
		return connection.FilterStreamRPCError(service.Run(ctx))
	}, service.WaitForReady)

	if len(conf.ServerConfigs) > 0 {
		require.Zero(t, conf.TestServerParameters.NumService)
		return service, test.StartGrpcServersWithConfigForTest(t.Context(), t, service.RegisterService,
			conf.ServerConfigs...,
		)
	}

	servers := test.StartGrpcServersForTest(t.Context(), t, conf.TestServerParameters, service.RegisterService)
	return service, servers
}

// OrdererTestEnv allows starting fake and holder services in addition to the regular mock orderer services.
type OrdererTestEnv struct {
	Orderer        *Orderer
	OrdererServers *test.GrpcServers
	FakeServers    *test.GrpcServers
	TestConfig     *OrdererTestConfig
}

// OrdererTestConfig describes the configuration for OrdererTestEnv.
type OrdererTestConfig struct {
	ChanID  string
	Config  *OrdererConfig
	NumFake int
}

// NewOrdererTestEnv creates and starts a new OrdererTestEnv.
func NewOrdererTestEnv(t *testing.T, conf *OrdererTestConfig) *OrdererTestEnv {
	t.Helper()
	orderer, ordererServers := StartMockOrderingServices(t, conf.Config)
	return &OrdererTestEnv{
		TestConfig:     conf,
		Orderer:        orderer,
		OrdererServers: ordererServers,
		FakeServers: test.StartGrpcServersForTest(
			t.Context(), t, test.StartServerParameters{
				NumService: conf.NumFake,
			}, nil,
		),
	}
}

// SubmitConfigBlock creates and submits a config block.
func (e *OrdererTestEnv) SubmitConfigBlock(t *testing.T, conf *workload.ConfigBlock) *common.Block {
	t.Helper()
	if conf == nil {
		conf = &workload.ConfigBlock{}
	}
	if conf.ChannelID == "" {
		conf.ChannelID = e.TestConfig.ChanID
	}
	if len(conf.OrdererEndpoints) == 0 {
		conf.OrdererEndpoints = e.AllEndpoints()
	}
	configBlock, err := workload.CreateDefaultConfigBlock(conf)
	require.NoError(t, err)
	err = e.Orderer.SubmitBlock(t.Context(), configBlock)
	require.NoError(t, err)
	return configBlock
}

// AllEndpoints returns a list of all the endpoints (real, fake, and holders).
func (e *OrdererTestEnv) AllEndpoints() []*commontypes.OrdererEndpoint {
	return slices.Concat(e.AllRealEndpoints(), e.AllFakeEndpoints())
}

// AllRealEndpoints returns a list of the real orderer endpoints.
func (e *OrdererTestEnv) AllRealEndpoints() []*commontypes.OrdererEndpoint {
	return test.NewOrdererEndpoints(0, e.OrdererServers.Configs...)
}

// AllFakeEndpoints returns a list of the fake orderer endpoints.
func (e *OrdererTestEnv) AllFakeEndpoints() []*commontypes.OrdererEndpoint {
	return test.NewOrdererEndpoints(0, e.FakeServers.Configs...)
}

// StreamFetcher is used by RequireStreams/RequireStreamsWithEndpoints.
type StreamFetcher[T any] interface {
	StreamsStates() []*T
	StreamsStatesByServerEndpoints(endpoint ...string) []*T
}

// RequireStreams ensures that there are a specified number of active streams.
func RequireStreams[T any, S StreamFetcher[T]](t *testing.T, manager S, expectedNumStreams int,
) []*T {
	t.Helper()
	var states []*T
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		states = manager.StreamsStates()
		require.Len(ct, states, expectedNumStreams)
	}, time.Minute, 10*time.Millisecond)
	return states
}

// RequireStreamsWithEndpoints ensures that there are a specified number of active streams via a specified endpoint.
func RequireStreamsWithEndpoints[T any, S StreamFetcher[T]](
	t *testing.T, manager S, expectedNumStreams int, endpoints ...string,
) []*T {
	t.Helper()
	var states []*T
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		states = manager.StreamsStatesByServerEndpoints(endpoints...)
		require.Len(ct, states, expectedNumStreams)
	}, time.Minute, 10*time.Millisecond)
	return states
}
