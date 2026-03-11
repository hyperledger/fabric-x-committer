/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package mock

import (
	"context"
	"path"
	"testing"
	"time"

	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	commontypes "github.com/hyperledger/fabric-x-common/api/types"
	"github.com/hyperledger/fabric-x-common/tools/cryptogen"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/ordererconn"
	"github.com/hyperledger/fabric-x-committer/utils/serialization"
	"github.com/hyperledger/fabric-x-committer/utils/test"
	"github.com/hyperledger/fabric-x-committer/utils/testcrypto"
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

// OrdererTestEnv prepares and controls the environment for testing the operation with the orderer.
type OrdererTestEnv struct {
	OrdererTestParameters
	OrdererConnConfig ordererconn.Config
	Orderer           *Orderer
	PrevBlock         *common.Block
	PartyStates       map[uint32]*PartyState
	AllServerConfig   []*connection.ServerConfig
	AllServers        []*grpc.Server
	AllEndpoints      []*commontypes.OrdererEndpoint
}

// OrdererTestParameters describes the parameters for OrdererTestEnv.
// Some fields are required, while others have default values if not set. See NewOrdererTestEnv for details.
type OrdererTestParameters struct {
	ArtifactsPath         string
	ChanID                string
	NumIDs                uint32
	ServerPerID           int
	PeerOrganizationCount uint32
	OrdererConfig         *OrdererConfig
	ServerTLSConfig       connection.TLSConfig
	ClientTLSConfig       connection.TLSConfig

	// NewOrdererTestEnv prepares the environment for the entire test.
	// However, the test might choose to start with a subset of the IDs, then
	// add the others as the test progresses.
	// We want to allow this without having to create servers in flight.
	// InitialNumIDs determines the initial number of IDs, to choose for the genesis block.
	// If not initialized, the default is all the IDs.
	InitialNumIDs uint32
}

// ConfigBlockPath is a convenient way to get the config block path.
func (p *OrdererTestParameters) ConfigBlockPath() string {
	return path.Join(p.ArtifactsPath, cryptogen.ConfigBlockFileName)
}

// NewOrdererTestEnv creates and starts a new OrdererTestEnv.
func NewOrdererTestEnv(t *testing.T, p *OrdererTestParameters) *OrdererTestEnv {
	t.Helper()
	if p.ArtifactsPath == "" {
		p.ArtifactsPath = t.TempDir()
	}
	if len(p.ChanID) == 0 {
		p.ChanID = "channel"
	}
	p.NumIDs = max(1, p.NumIDs)
	p.ServerPerID = max(1, p.ServerPerID)
	p.PeerOrganizationCount = max(1, p.PeerOrganizationCount)

	instanceCount := int(p.NumIDs) * p.ServerPerID
	t.Logf("Orderer instances: %d; IDs: %d", instanceCount, p.NumIDs)

	allServerConfigs := make([]*connection.ServerConfig, 0, instanceCount)
	allEndpoints := make([]*commontypes.OrdererEndpoint, 0, instanceCount)
	partyStates := make(map[uint32]*PartyState, p.NumIDs)
	for id := range p.NumIDs {
		partyStates[id] = &PartyState{PartyID: id}
		for range p.ServerPerID {
			server := test.NewPreAllocatedLocalHostServer(t, p.ServerTLSConfig)
			allServerConfigs = append(allServerConfigs, server)
			allEndpoints = append(allEndpoints, &commontypes.OrdererEndpoint{
				ID:   id,
				Host: server.Endpoint.Host,
				Port: server.Endpoint.Port,
			})
		}
	}
	for i, e := range allEndpoints {
		t.Logf("ORDERER ENDPOINT [%02d] %s", i, e)
	}

	_, err := testcrypto.CreateOrExtendConfigBlockWithCrypto(p.ArtifactsPath, &testcrypto.ConfigBlock{
		ChannelID:             p.ChanID,
		OrdererEndpoints:      filterEndpoints(allEndpoints, p.InitialNumIDs),
		PeerOrganizationCount: p.PeerOrganizationCount,
	})
	require.NoError(t, err)
	p.OrdererConfig.ArtifactsPath = p.ArtifactsPath

	// Start the system.
	ordererService, err := NewMockOrderer(p.OrdererConfig)
	require.NoError(t, err)
	// Register the party states.
	for _, ep := range allEndpoints {
		ordererService.RegisterPartyState(ep.Address(), partyStates[ep.ID])
	}

	ctx := t.Context()
	test.RunServiceForTest(ctx, t, func(ctx context.Context) error {
		return connection.FilterStreamRPCError(ordererService.Run(ctx))
	}, ordererService.WaitForReady)

	e := &OrdererTestEnv{
		OrdererTestParameters: *p,
		AllServerConfig:       allServerConfigs,
		AllEndpoints:          allEndpoints,
		OrdererConnConfig:     testcrypto.GetOrdererConnConfig(p.ArtifactsPath, p.ClientTLSConfig),
		PartyStates:           partyStates,
		Orderer:               ordererService,
	}
	e.StartServers(t)
	return e
}

// StartServers starts the servers for the orderer and maps them to their respective IDs.
func (e *OrdererTestEnv) StartServers(t *testing.T) {
	t.Helper()
	allServers := test.StartGrpcServersWithConfigForTest(
		t.Context(), t, e.Orderer.RegisterService, e.AllServerConfig...,
	)
	require.Len(t, allServers.Servers, len(e.AllServerConfig))
	e.AllServers = allServers.Servers
}

// StopServers stops the servers and closes their pre allocated listeners.
func (e *OrdererTestEnv) StopServers() {
	for _, s := range e.AllServers {
		s.Stop()
	}
	for _, s := range e.AllServerConfig {
		_ = s.ClosePreAllocatedListener()
	}
}

// StopServersOfID stop the servers of a party ID and closes their pre allocated listeners.
func (e *OrdererTestEnv) StopServersOfID(id uint32) {
	for idx, ep := range e.AllEndpoints {
		if ep.ID == id {
			e.AllServers[idx].Stop()
			_ = e.AllServerConfig[idx].ClosePreAllocatedListener()
		}
	}
}

// EndpointsOfID returns the OrdererEndpoint of a party ID.
func (e *OrdererTestEnv) EndpointsOfID(id uint32) []*commontypes.OrdererEndpoint {
	endpoints := make([]*commontypes.OrdererEndpoint, 0, len(e.AllEndpoints))
	for _, ep := range e.AllEndpoints {
		if ep.ID == id {
			endpoints = append(endpoints, ep)
		}
	}
	return endpoints
}

func filterEndpoints(allEndpoints []*commontypes.OrdererEndpoint, maxID uint32) []*commontypes.OrdererEndpoint {
	if maxID == 0 {
		return allEndpoints
	}
	filteredEndpoints := make([]*commontypes.OrdererEndpoint, 0, len(allEndpoints))
	for _, ep := range allEndpoints {
		if ep.ID < maxID {
			filteredEndpoints = append(filteredEndpoints, ep)
		}
	}
	return filteredEndpoints
}

// SubmitConfigBlock creates and submits a config block.
func (e *OrdererTestEnv) SubmitConfigBlock(
	t *testing.T, c *testcrypto.ConfigBlock,
) *ordererconn.ConfigBlockMaterial {
	t.Helper()
	if c == nil {
		c = &testcrypto.ConfigBlock{}
	}
	prevConfigMaterial, err := ordererconn.LoadConfigBlockFromFile(e.ConfigBlockPath())
	require.NoError(t, err)
	if len(c.ChannelID) == 0 {
		c.ChannelID = prevConfigMaterial.ChannelID
	}
	if len(c.OrdererEndpoints) == 0 {
		for _, m := range prevConfigMaterial.OrdererOrganizations {
			c.OrdererEndpoints = append(c.OrdererEndpoints, m.Endpoints...)
		}
	}
	if c.PeerOrganizationCount == 0 {
		c.PeerOrganizationCount = uint32(len(prevConfigMaterial.ApplicationOrganizations)) // #nosec G115
	}

	configBlock, err := testcrypto.CreateOrExtendConfigBlockWithCrypto(e.ArtifactsPath, c)
	require.NoError(t, err)
	configMaterial, err := ordererconn.LoadConfigBlock(configBlock)
	require.NoError(t, err)

	consenters, err := testcrypto.GetConsenterIdentities(e.ArtifactsPath)
	require.NoError(t, err)

	err = e.Orderer.SubmitBlockWithConsenters(t.Context(), &BlockWithConsenters{
		Block:            configMaterial.ConfigBlock,
		ConsenterSigners: consenters,
	})
	require.NoError(t, err)
	return configMaterial
}

// GetBlock returns a block number.
func (e *OrdererTestEnv) GetBlock(t *testing.T, blockNum uint64) *common.Block {
	t.Helper()
	b, err := e.Orderer.GetBlock(t.Context(), blockNum)
	require.NoError(t, err)
	require.NotNil(t, b)
	return b
}

// Submit submits a block and verify its delivery.
func (e *OrdererTestEnv) Submit(
	t *testing.T, outputBlocks <-chan *common.Block,
) *common.Block {
	t.Helper()
	txs := workload.GenerateTransactions(t, nil, 10)

	block := workload.MapToOrdererBlock(0, txs)
	err := e.Orderer.SubmitBlock(t.Context(), block)
	require.NoError(t, err)

	b := e.WaitForBlock(t, outputBlocks)

	require.Len(t, b.Data.Data, len(txs))
	for i, d := range b.Data.Data {
		_, hdr, marshalErr := serialization.UnwrapEnvelope(d)
		require.NoError(t, marshalErr)
		require.Equal(t, txs[i].Id, hdr.TxId)
	}
	return b
}

// WaitForBlock waits for a block on the channel.
func (e *OrdererTestEnv) WaitForBlock(t *testing.T, outputBlocks <-chan *common.Block) *common.Block {
	t.Helper()
	b, ok := channel.NewReader(t.Context(), outputBlocks).ReadWithTimeout(3 * time.Second)
	require.True(t, ok, "failed to receive block from output channel")
	require.NotNil(t, b)
	if e.PrevBlock != nil {
		require.Equal(t, e.PrevBlock.Header.Number+1, b.Header.Number)
	}
	t.Logf("Received block #%d", b.Header.Number)
	e.PrevBlock = b
	return b
}
