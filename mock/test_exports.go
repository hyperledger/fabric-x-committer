/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package mock

import (
	"context"
	"slices"
	"testing"

	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/ordererconn"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

// StartMockSVService starts a specified number of mock verifier service and register cancellation.
func StartMockSVService(t *testing.T, numService int) (
	[]*SigVerifier, *test.GrpcServers,
) {
	t.Helper()
	mockSigVer := make([]*SigVerifier, numService)
	for i := range numService {
		mockSigVer[i] = NewMockSigVerifier()
	}

	sigVerServers := test.StartGrpcServersForTest(t.Context(), t, len(mockSigVer),
		func(server *grpc.Server, index int) {
			mockSigVer[index].RegisterService(server)
		})
	return mockSigVer, sigVerServers
}

// StartMockSVServiceFromListWithConfig starts a specified number of mock verifier service.
func StartMockSVServiceFromListWithConfig(
	t *testing.T, svs []*SigVerifier, sc []*connection.ServerConfig,
) *test.GrpcServers {
	t.Helper()
	return test.StartGrpcServersWithConfigForTest(t.Context(), t, sc, func(server *grpc.Server, index int) {
		svs[index].RegisterService(server)
	})
}

// StartMockVCService starts a specified number of mock VC service and register cancellation.
func StartMockVCService(t *testing.T, numService int) (
	[]*VcService, *test.GrpcServers,
) {
	t.Helper()
	vcServices := make([]*VcService, numService)
	for i := range numService {
		vcServices[i] = NewMockVcService()
	}

	vcGrpc := test.StartGrpcServersForTest(t.Context(), t, numService, func(server *grpc.Server, index int) {
		vcServices[index].RegisterService(server)
	})
	return vcServices, vcGrpc
}

// StartMockVCServiceFromListWithConfig starts a specified number of mock vc service.
func StartMockVCServiceFromListWithConfig(
	t *testing.T, vcs []*VcService, sc []*connection.ServerConfig,
) *test.GrpcServers {
	t.Helper()
	return test.StartGrpcServersWithConfigForTest(t.Context(), t, sc, func(server *grpc.Server, index int) {
		vcs[index].RegisterService(server)
	})
}

// StartMockCoordinatorService starts a mock coordinator service and registers cancellation.
func StartMockCoordinatorService(t *testing.T) (
	*Coordinator, *test.GrpcServers,
) {
	t.Helper()
	mockCoordinator := NewMockCoordinator()
	coordinatorGrpc := test.StartGrpcServersForTest(t.Context(), t, 1, func(server *grpc.Server, _ int) {
		mockCoordinator.RegisterService(server)
	})
	return mockCoordinator, coordinatorGrpc
}

// StartMockCoordinatorServiceFromListWithConfig starts a mock coordinator service using the given config.
func StartMockCoordinatorServiceFromListWithConfig(
	t *testing.T,
	coordService *Coordinator,
	sc *connection.ServerConfig,
) *test.GrpcServers {
	t.Helper()
	return test.StartGrpcServersWithConfigForTest(t.Context(), t, []*connection.ServerConfig{sc},
		func(server *grpc.Server, _ int) {
			coordService.RegisterService(server)
		})
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

	if len(conf.ServerConfigs) == conf.NumService {
		return service, test.StartGrpcServersWithConfigForTest(
			t.Context(),
			t, conf.ServerConfigs, func(server *grpc.Server, _ int) {
				service.RegisterService(server)
			},
		)
	}

	servers := test.StartGrpcServersForTest(
		t.Context(), t, conf.NumService, func(server *grpc.Server, _ int) {
			service.RegisterService(server)
		},
	)
	return service, servers
}

// OrdererTestEnv allows starting fake and holder services in addition to the regular mock orderer services.
type OrdererTestEnv struct {
	Orderer        *Orderer
	Holder         *HoldingOrderer
	OrdererServers *test.GrpcServers
	FakeServers    *test.GrpcServers
	HolderServers  *test.GrpcServers
	TestConfig     *OrdererTestConfig
}

// OrdererTestConfig describes the configuration for OrdererTestEnv.
type OrdererTestConfig struct {
	ChanID                       string
	Config                       *OrdererConfig
	NumFake                      int
	NumHolders                   int
	MetaNamespaceVerificationKey []byte
}

// NewOrdererTestEnv creates and starts a new OrdererTestEnv.
func NewOrdererTestEnv(t *testing.T, conf *OrdererTestConfig) *OrdererTestEnv {
	t.Helper()
	orderer, ordererServers := StartMockOrderingServices(t, conf.Config)
	holder := &HoldingOrderer{Orderer: orderer}
	holder.Release()
	return &OrdererTestEnv{
		TestConfig:     conf,
		Orderer:        orderer,
		Holder:         holder,
		OrdererServers: ordererServers,
		HolderServers: test.StartGrpcServersForTest(t.Context(), t, conf.NumHolders, func(s *grpc.Server, _ int) {
			holder.RegisterService(s)
		}),
		FakeServers: test.StartGrpcServersForTest(t.Context(), t, conf.NumFake, nil),
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
	if conf.MetaNamespaceVerificationKey == nil {
		conf.MetaNamespaceVerificationKey = e.TestConfig.MetaNamespaceVerificationKey
	}
	configBlock, err := workload.CreateDefaultConfigBlock(conf)
	require.NoError(t, err)
	e.Orderer.SubmitBlock(t.Context(), configBlock)
	return configBlock
}

// AllEndpoints returns a list of all the endpoints (real, fake, and holders).
func (e *OrdererTestEnv) AllEndpoints() []*ordererconn.Endpoint {
	return slices.Concat(
		e.AllRealOrdererEndpoints(),
		e.AllHolderEndpoints(),
		e.AllFakeEndpoints(),
	)
}

// AllRealOrdererEndpoints returns a list of the real orderer endpoints.
func (e *OrdererTestEnv) AllRealOrdererEndpoints() []*ordererconn.Endpoint {
	return ordererconn.NewEndpoints(0, "org", e.OrdererServers.Configs...)
}

// AllFakeEndpoints returns a list of the fake orderer endpoints.
func (e *OrdererTestEnv) AllFakeEndpoints() []*ordererconn.Endpoint {
	return ordererconn.NewEndpoints(0, "org", e.FakeServers.Configs...)
}

// AllHolderEndpoints returns a list of the holder orderer endpoints.
func (e *OrdererTestEnv) AllHolderEndpoints() []*ordererconn.Endpoint {
	return ordererconn.NewEndpoints(0, "org", e.HolderServers.Configs...)
}
