/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package runner

import (
	"context"
	"fmt"
	"math/rand"
	"path/filepath"
	"testing"
	"time"

	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	commontypes "github.com/hyperledger/fabric-x-common/api/types"
	"github.com/hyperledger/fabric-x-common/tools/configtxgen"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/cmd/config"
	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/service/sidecar"
	"github.com/hyperledger/fabric-x-committer/service/sidecar/sidecarclient"
	"github.com/hyperledger/fabric-x-committer/service/vc"
	"github.com/hyperledger/fabric-x-committer/service/vc/dbtest"
	"github.com/hyperledger/fabric-x-committer/utils/apptest"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/deliver"
	"github.com/hyperledger/fabric-x-committer/utils/logging"
	"github.com/hyperledger/fabric-x-committer/utils/ordererconn"
	"github.com/hyperledger/fabric-x-committer/utils/serialization"
	"github.com/hyperledger/fabric-x-committer/utils/signature"
	"github.com/hyperledger/fabric-x-committer/utils/signature/sigtest"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

type (
	// CommitterRuntime represents a test system of Coordinator, SigVerifier, VCService and Query processes.
	CommitterRuntime struct {
		SystemConfig config.SystemConfig

		MockOrderer  *ProcessWithConfig
		Sidecar      *ProcessWithConfig
		Coordinator  *ProcessWithConfig
		QueryService *ProcessWithConfig
		Verifier     []*ProcessWithConfig
		VcService    []*ProcessWithConfig

		DBEnv *vc.DatabaseTestEnv

		OrdererStream      *test.BroadcastStream
		CoordinatorClient  servicepb.CoordinatorClient
		QueryServiceClient committerpb.QueryServiceClient
		SidecarClient      *sidecarclient.Client
		NotifyClient       committerpb.NotifierClient
		NotifyStream       committerpb.Notifier_OpenNotificationStreamClient

		CommittedBlock chan *common.Block

		TxBuilder *workload.TxBuilder

		Config *Config

		SeedForCryptoGen *rand.Rand

		LastReceivedBlockNumber uint64

		CredFactory *test.CredentialsFactory
	}

	// Config represents the runtime configuration.
	Config struct {
		NumVerifiers      int
		NumVCService      int
		BlockSize         uint64
		BlockTimeout      time.Duration
		LoadgenBlockLimit uint64

		// DBConnection configures the runtime to operate with a custom database connection.
		DBConnection *dbtest.Connection
		// TLS configures the secure level between the components: none | tls | mtls
		TLSMode string
		// CrashTest is true to indicate a service crash is expected, and not a failure.
		CrashTest bool
		// RateLimit configures rate limiting for services that support it (query, sidecar).
		RateLimit *connection.RateLimitConfig
		// MaxRequestKeys is the maximum number of keys allowed in a single query request.
		// Set to 0 to disable the limit.
		MaxRequestKeys int

		// VCMinTransactionBatchSize configures the minimum batch size for VC service.
		VCMinTransactionBatchSize int
		// VCTimeoutForMinTransactionBatchSize configures the timeout for min batch size
		VCTimeoutForMinTransactionBatchSize time.Duration

		// VerifierBatchTimeCutoff configures the batch time cutoff for verifier service
		VerifierBatchTimeCutoff time.Duration
		// VerifierBatchSizeCutoff configures the batch size cutoff for verifier service.
		VerifierBatchSizeCutoff int
	}
)

// Service flags.
const (
	Orderer = 1 << iota
	Sidecar
	Coordinator
	Verifier
	VC
	QueryService

	LoadGenForOnlyOrderer
	LoadGenForOrderer
	LoadGenForCommitter
	LoadGenForCoordinator
	LoadGenForVerifier
	LoadGenForVCService
	LoadGenForDistributedLoadGen

	CommitterTxPath       = Sidecar | Coordinator | Verifier | VC
	FullTxPath            = Orderer | CommitterTxPath
	FullTxPathWithLoadGen = FullTxPath | LoadGenForOrderer
	FullTxPathWithQuery   = FullTxPath | QueryService

	CommitterTxPathWithLoadGen = CommitterTxPath | LoadGenForCommitter

	// loadGenMatcher is used to extract only the load generator flags from the full service flags value.
	loadGenMatcher = LoadGenForOnlyOrderer | LoadGenForOrderer | LoadGenForCommitter | LoadGenForCoordinator |
		LoadGenForVCService | LoadGenForVerifier

	TestChannelName = "channel1"
)

// NewRuntime creates a new test runtime.
func NewRuntime(t *testing.T, conf *Config) *CommitterRuntime {
	t.Helper()

	c := &CommitterRuntime{
		Config: conf,
		SystemConfig: config.SystemConfig{
			BlockSize:         conf.BlockSize,
			BlockTimeout:      conf.BlockTimeout,
			LoadGenBlockLimit: conf.LoadgenBlockLimit,
			LoadGenWorkers:    1,
			MaxRequestKeys:    conf.MaxRequestKeys,
			Policy: &workload.PolicyProfile{
				ChannelID:          TestChannelName,
				NamespacePolicies:  make(map[string]*workload.Policy),
				CryptoMaterialPath: t.TempDir(),
			},
			Logging:   &logging.DefaultConfig,
			RateLimit: conf.RateLimit,

			// Batching configuration for testing.
			VCMinTransactionBatchSize:           conf.VCMinTransactionBatchSize,
			VCTimeoutForMinTransactionBatchSize: conf.VCTimeoutForMinTransactionBatchSize,
			VerifierBatchTimeCutoff:             conf.VerifierBatchTimeCutoff,
			VerifierBatchSizeCutoff:             conf.VerifierBatchSizeCutoff,
		},
		CommittedBlock:   make(chan *common.Block, 100),
		SeedForCryptoGen: rand.New(rand.NewSource(10)),
	}
	c.AddOrUpdateNamespaces(t, committerpb.MetaNamespaceID, workload.DefaultGeneratedNamespaceID, "1", "2", "3")

	t.Log("Making DB env")
	if conf.DBConnection == nil {
		c.DBEnv = vc.NewDatabaseTestEnv(t)
	} else {
		c.DBEnv = vc.NewDatabaseTestEnvWithCustomConnection(t, conf.DBConnection)
	}

	s := &c.SystemConfig
	s.DB.Name = c.DBEnv.DBConf.Database
	s.DB.Password = c.DBEnv.DBConf.Password
	s.DB.LoadBalance = c.DBEnv.DBConf.LoadBalance
	s.DB.Endpoints = c.DBEnv.DBConf.Endpoints
	s.DB.TLS = c.DBEnv.DBConf.TLS
	s.LedgerPath = t.TempDir()
	s.ConfigBlockPath = filepath.Join(t.TempDir(), "config-block.pb.bin")

	t.Log("Allocating ports")
	ports := portAllocator{}
	defer ports.close()
	s.Endpoints.Orderer = ports.allocatePorts(t, 1)
	s.Endpoints.Verifier = ports.allocatePorts(t, conf.NumVerifiers)
	s.Endpoints.VCService = ports.allocatePorts(t, conf.NumVCService)
	s.Endpoints.Query = ports.allocatePorts(t, 1)[0]
	s.Endpoints.Coordinator = ports.allocatePorts(t, 1)[0]
	s.Endpoints.Sidecar = ports.allocatePorts(t, 1)[0]
	s.Endpoints.LoadGen = ports.allocatePorts(t, 1)[0]
	s.Policy.OrdererEndpoints = make([]*commontypes.OrdererEndpoint, len(s.Endpoints.Orderer))
	for i, e := range s.Endpoints.Orderer {
		s.Policy.OrdererEndpoints[i] = &commontypes.OrdererEndpoint{
			ID: 0, MspID: "org", Host: e.Server.Host, Port: e.Server.Port,
			API: []string{commontypes.Broadcast, commontypes.Deliver},
		}
	}

	test.LogStruct(t, "System Parameters", s)

	t.Log("Creating config block")
	configBlock, err := workload.CreateConfigBlock(s.Policy)
	require.NoError(t, err)
	err = configtxgen.WriteOutputBlock(configBlock, s.ConfigBlockPath)
	require.NoError(t, err)

	t.Log("create TLS manager and clients certificate")
	c.CredFactory = test.NewCredentialsFactory(t)
	s.ClientTLS, _ = c.CredFactory.CreateClientCredentials(t, c.Config.TLSMode)

	t.Log("Create processes")
	c.MockOrderer = newProcess(t, cmdOrderer, c.systemConfigWithServerTLS(t, *s, s.Endpoints.Orderer[0]))
	for i, e := range s.Endpoints.Verifier {
		p := cmdVerifier
		p.Name = fmt.Sprintf("%s-%d", p.Name, i)
		// we generate different keys for each verifier.
		c.Verifier = append(c.Verifier, newProcess(t, p, c.systemConfigWithServerTLS(t, *s, e)))
	}

	for i, e := range s.Endpoints.VCService {
		p := cmdVC
		p.Name = fmt.Sprintf("%s-%d", p.Name, i)
		// we generate different keys for each vc-service.
		c.VcService = append(c.VcService, newProcess(t, p, c.systemConfigWithServerTLS(t, *s, e)))
	}

	c.Coordinator = newProcess(t, cmdCoordinator, c.systemConfigWithServerTLS(t, *s, s.Endpoints.Coordinator))

	c.QueryService = newProcess(t, cmdQuery, c.systemConfigWithServerTLS(t, *s, s.Endpoints.Query))

	c.Sidecar = newProcess(t, cmdSidecar, c.systemConfigWithServerTLS(t, *s, s.Endpoints.Sidecar))

	t.Log("Create clients")
	c.CreateRuntimeClients(t.Context(), t)
	return c
}

// CreateRuntimeClients create and set the necessary service's clients.
func (c *CommitterRuntime) CreateRuntimeClients(ctx context.Context, t *testing.T) {
	t.Helper()
	endpoints := c.SystemConfig.Endpoints

	c.CoordinatorClient = servicepb.NewCoordinatorClient(
		test.NewSecuredConnection(t, endpoints.Coordinator.Server, c.SystemConfig.ClientTLS),
	)

	c.QueryServiceClient = committerpb.NewQueryServiceClient(
		test.NewSecuredConnection(t, endpoints.Query.Server, c.SystemConfig.ClientTLS),
	)

	c.NotifyClient = committerpb.NewNotifierClient(
		test.NewSecuredConnection(t, endpoints.Sidecar.Server, c.SystemConfig.ClientTLS),
	)

	var err error
	c.OrdererStream, err = test.NewBroadcastStream(ctx, &ordererconn.Config{
		TLS:           ordererconn.TLSConfigToOrdererTLSConfig(c.SystemConfig.ClientTLS),
		ChannelID:     c.SystemConfig.Policy.ChannelID,
		Identity:      c.SystemConfig.Policy.Identity,
		ConsensusType: ordererconn.Bft,
		Organizations: map[string]*ordererconn.OrganizationConfig{
			"org": {
				Endpoints: c.SystemConfig.Policy.OrdererEndpoints,
				CACerts:   c.SystemConfig.ClientTLS.CACertPaths,
			},
		},
	})
	require.NoError(t, err)
	t.Cleanup(c.OrdererStream.CloseConnections)

	c.SidecarClient, err = sidecarclient.New(&sidecarclient.Parameters{
		ChannelID: c.SystemConfig.Policy.ChannelID,
		Client:    test.NewTLSClientConfig(c.SystemConfig.ClientTLS, endpoints.Sidecar.Server),
	})
	require.NoError(t, err)
	t.Cleanup(c.SidecarClient.CloseConnections)
}

// OpenNotificationStream starts a notification stream.
func (c *CommitterRuntime) OpenNotificationStream(ctx context.Context, t *testing.T) {
	t.Helper()
	var err error
	c.NotifyStream, err = c.NotifyClient.OpenNotificationStream(ctx)
	require.NoError(t, err)
}

// Start runs all services and load generator as configured by the serviceFlags.
func (c *CommitterRuntime) Start(t *testing.T, serviceFlags int) {
	t.Helper()

	require.Falsef(t, isMoreThanOneBitSet((Orderer|LoadGenForCommitter)&serviceFlags),
		"cannot use load generator for committer with an orderer")

	t.Log("Running services")
	if loadGenMatcher&serviceFlags != 0 {
		c.startLoadGen(t, serviceFlags)
	}
	if Orderer&serviceFlags != 0 {
		c.MockOrderer.Restart(t)
	}
	if Verifier&serviceFlags != 0 {
		for _, p := range c.Verifier {
			p.Restart(t)
		}
	}
	if VC&serviceFlags != 0 {
		for _, p := range c.VcService {
			p.Restart(t)
		}
	}
	if Coordinator&serviceFlags != 0 {
		c.Coordinator.Restart(t)
	}
	if Sidecar&serviceFlags != 0 {
		c.Sidecar.Restart(t)
		c.OpenNotificationStream(t.Context(), t)
	}
	if QueryService&serviceFlags != 0 {
		c.QueryService.Restart(t)
	}

	if Coordinator&serviceFlags != 0 && Sidecar&serviceFlags != 0 {
		// We need the sidecar to update the coordinator with the last committed block.
		t.Log("Validate coordinator state")
		c.ensureAtLeastLastCommittedBlockNumber(t, 0)
	}

	if Sidecar&serviceFlags != 0 {
		c.startBlockDelivery(t)
	}
}

func (c *CommitterRuntime) startLoadGen(t *testing.T, serviceFlags int) {
	t.Helper()
	loadGenFlag := loadGenMatcher & serviceFlags
	require.Falsef(t, isMoreThanOneBitSet(loadGenFlag), "only one load generator may be set")
	loadGenParams := cmdLoadGen
	switch loadGenFlag {
	case LoadGenForOnlyOrderer:
		loadGenParams.Template = config.TemplateLoadGenOnlyOrderer
	case LoadGenForOrderer:
		loadGenParams.Template = config.TemplateLoadGenOrderer
	case LoadGenForCommitter:
		loadGenParams.Template = config.TemplateLoadGenCommitter
	case LoadGenForCoordinator:
		loadGenParams.Template = config.TemplateLoadGenCoordinator
	case LoadGenForVCService:
		loadGenParams.Template = config.TemplateLoadGenVC
	case LoadGenForVerifier:
		loadGenParams.Template = config.TemplateLoadGenVerifier
	default:
		return
	}
	s := c.SystemConfig

	isDist := serviceFlags&LoadGenForDistributedLoadGen != 0
	if isDist {
		s.LoadGenWorkers = 0
	}
	newProcess(t, loadGenParams, c.systemConfigWithServerTLS(t, s, s.Endpoints.LoadGen)).Restart(t)
	if isDist {
		s.LoadGenWorkers = 1
		loadGenParams.Name = "dist-loadgen"
		loadGenParams.Template = config.TemplateLoadGenDistributedLoadGenClient
		newProcess(t, loadGenParams, systemConfigWithEndpoint(s, config.ServiceEndpoints{})).Restart(t)
	}
}

func (c *CommitterRuntime) startBlockDelivery(t *testing.T) {
	t.Helper()
	t.Log("Running delivery client")
	test.RunServiceForTest(t.Context(), t, func(ctx context.Context) error {
		return connection.FilterStreamRPCError(c.SidecarClient.Deliver(ctx, &sidecarclient.DeliverParameters{
			EndBlkNum:   deliver.MaxBlockNum,
			OutputBlock: c.CommittedBlock,
		}))
	}, func(ctx context.Context) bool {
		select {
		case <-ctx.Done():
			return false
		case b := <-c.CommittedBlock:
			require.NotNil(t, b)
			return true
		}
	})
}

// AddOrUpdateNamespaces adds policies for namespaces. If already exists, the policy will be updated.
func (c *CommitterRuntime) AddOrUpdateNamespaces(t *testing.T, namespaces ...string) {
	t.Helper()
	for _, ns := range namespaces {
		c.SystemConfig.Policy.NamespacePolicies[ns] = &workload.Policy{
			Scheme: signature.Ecdsa,
			Seed:   c.SeedForCryptoGen.Int63(),
		}
	}
	var err error
	c.TxBuilder, err = workload.NewTxBuilderFromPolicy(c.SystemConfig.Policy, nil)
	require.NoError(t, err)
}

// CreateNamespacesAndCommit creates namespaces in the committer.
func (c *CommitterRuntime) CreateNamespacesAndCommit(t *testing.T, namespaces ...string) {
	t.Helper()
	if len(namespaces) == 0 {
		return
	}

	t.Logf("Creating namespaces: %v", namespaces)
	metaTX, err := workload.CreateNamespacesTX(c.SystemConfig.Policy, 0, namespaces...)
	require.NoError(t, err)
	c.MakeAndSendTransactionsToOrderer(
		t,
		[][]*applicationpb.TxNamespace{metaTX.Namespaces},
		[]committerpb.Status{committerpb.Status_COMMITTED},
	)
}

// MakeAndSendTransactionsToOrderer creates a block with given transactions, send it to the committer,
// and verify the result.
func (c *CommitterRuntime) MakeAndSendTransactionsToOrderer(
	t *testing.T, txsNs [][]*applicationpb.TxNamespace, expectedStatus []committerpb.Status,
) []string {
	t.Helper()
	txs := make([]*servicepb.LoadGenTx, len(txsNs))

	for i, namespaces := range txsNs {
		tx := &applicationpb.Tx{
			Namespaces: namespaces,
		}
		if expectedStatus != nil && expectedStatus[i] == committerpb.Status_ABORTED_SIGNATURE_INVALID {
			tx.Endorsements = make([]*applicationpb.Endorsements, len(namespaces))
			for nsIdx := range namespaces {
				tx.Endorsements[nsIdx] = sigtest.CreateEndorsementsForThresholdRule([]byte("dummy"))[0]
			}
		}
		txs[i] = c.TxBuilder.MakeTx(tx)
	}

	return c.SendTransactionsToOrderer(t, txs, expectedStatus)
}

// SendTransactionsToOrderer creates a block with given transactions, send it to the committer, and verify the result.
func (c *CommitterRuntime) SendTransactionsToOrderer(
	t *testing.T, txs []*servicepb.LoadGenTx, expectedStatus []committerpb.Status,
) []string {
	t.Helper()
	expected := &ExpectedStatusInBlock{
		Statuses: expectedStatus,
		TxIDs:    make([]string, len(txs)),
	}
	for i, tx := range txs {
		expected.TxIDs[i] = tx.Id
	}

	if !c.Config.CrashTest {
		err := c.NotifyStream.Send(&committerpb.NotificationRequest{
			TxStatusRequest: &committerpb.TxIDsBatch{
				TxIds: expected.TxIDs,
			},
			Timeout: durationpb.New(3 * time.Minute),
		})
		require.NoError(t, err)
		// Allows processing the request before submitting the payload.
		time.Sleep(1 * time.Second)
	}

	err := c.OrdererStream.SendBatch(workload.MapToEnvelopeBatch(0, txs))
	require.NoError(t, err)

	if expectedStatus != nil {
		c.ValidateExpectedResultsInCommittedBlock(t, expected)
	}
	return expected.TxIDs
}

// ExpectedStatusInBlock holds pairs of expected txID and the corresponding status in a block. The order of statuses
// is expected to be the same as in the committed block.
type ExpectedStatusInBlock struct {
	TxIDs    []string
	Statuses []committerpb.Status
}

// ValidateExpectedResultsInCommittedBlock validates the status of transactions in the committed block.
func (c *CommitterRuntime) ValidateExpectedResultsInCommittedBlock(t *testing.T, expected *ExpectedStatusInBlock) {
	t.Helper()
	var blk *common.Block
	var ok bool
	select {
	case blk, ok = <-c.CommittedBlock:
		if !ok {
			return
		}
	case <-time.After(2 * time.Minute):
		t.Fatalf("Timed out waiting for block #%d", c.LastReceivedBlockNumber+1)
	}
	c.LastReceivedBlockNumber = blk.Header.Number
	t.Logf("Got block #%d", blk.Header.Number)

	for txNum, txEnv := range blk.Data.Data {
		txBytes, hdr, err := serialization.UnwrapEnvelope(txEnv)
		require.NoError(t, err)
		require.NotNil(t, hdr)
		if hdr.Type == int32(common.HeaderType_CONFIG) {
			continue
		}
		_, err = serialization.UnmarshalTx(txBytes)
		require.NoError(t, err)
		require.Equal(t, expected.TxIDs[txNum], hdr.TxId)
	}

	expectedStatuses := make([]string, len(expected.Statuses))
	for i, s := range expected.Statuses {
		expectedStatuses[i] = s.String()
	}
	require.NotNil(t, blk.Metadata)
	require.Greater(t, len(blk.Metadata.Metadata), int(common.BlockMetadataIndex_TRANSACTIONS_FILTER))
	statusBytes := blk.Metadata.Metadata[common.BlockMetadataIndex_TRANSACTIONS_FILTER]
	actualStatuses := make([]string, len(statusBytes))
	for i, sB := range statusBytes {
		actualStatuses[i] = committerpb.Status(sB).String()
	}
	require.Equal(t, expectedStatuses, actualStatuses)

	c.ensureLastCommittedBlockNumber(t, blk.Header.Number)

	var persistedTxIDs []string
	persistedTxIDsStatus := make([]*committerpb.TxStatus, 0, len(expected.TxIDs))
	duplicateTxIDsStatus := make([]*committerpb.TxStatus, 0, len(expected.TxIDs))
	for i, tID := range expected.TxIDs {
		//nolint:gosec // int -> uint32.
		s := committerpb.NewTxStatus(expected.Statuses[i], tID, blk.Header.Number, uint32(i))
		if s.Status == committerpb.Status_REJECTED_DUPLICATE_TX_ID {
			duplicateTxIDsStatus = append(duplicateTxIDsStatus, s)
		}

		if sidecar.IsStatusStoredInDB(s.Status) {
			persistedTxIDsStatus = append(persistedTxIDsStatus, s)
			persistedTxIDs = append(persistedTxIDs, tID)
		}
	}

	c.DBEnv.StatusExistsForNonDuplicateTxID(t.Context(), t, persistedTxIDsStatus)
	// For the duplicate txID, neither the status nor the height would match the entry in the
	// transaction status table.
	c.DBEnv.StatusExistsWithDifferentHeightForDuplicateTxID(t, duplicateTxIDsStatus)

	ctx, cancel := context.WithTimeout(t.Context(), 1*time.Minute)
	defer cancel()
	apptest.EnsurePersistedTxStatus(ctx, t, c.CoordinatorClient, persistedTxIDs, persistedTxIDsStatus)

	if len(expected.TxIDs) == 0 || c.Config.CrashTest {
		return
	}

	sidecar.RequireNotifications(t, c.NotifyStream, blk.Header.Number, expected.TxIDs, expected.Statuses)
}

// CountStatus returns the number of transactions with a given tx status.
func (c *CommitterRuntime) CountStatus(t *testing.T, status committerpb.Status) int {
	t.Helper()
	return c.DBEnv.CountStatus(t, status)
}

// CountAlternateStatus returns the number of transactions not with a given tx status.
func (c *CommitterRuntime) CountAlternateStatus(t *testing.T, status committerpb.Status) int {
	t.Helper()
	return c.DBEnv.CountAlternateStatus(t, status)
}

func (c *CommitterRuntime) ensureLastCommittedBlockNumber(t *testing.T, blkNum uint64) {
	t.Helper()
	c.ensureAtLeastLastCommittedBlockNumber(t, blkNum)

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	nextBlock, err := c.CoordinatorClient.GetNextBlockNumberToCommit(ctx, nil)
	require.NoError(t, err)
	require.NotNil(t, nextBlock)
	require.Equal(t, blkNum+1, nextBlock.Number)
}

func (c *CommitterRuntime) ensureAtLeastLastCommittedBlockNumber(t *testing.T, blkNum uint64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		nextBlock, err := c.CoordinatorClient.GetNextBlockNumberToCommit(ctx, nil)
		require.NoError(ct, err)
		require.NotNil(ct, nextBlock)
		require.Greater(ct, nextBlock.Number, blkNum)
	}, 2*time.Minute, 250*time.Millisecond)
}

func (c *CommitterRuntime) systemConfigWithServerTLS(
	t *testing.T,
	systemConf config.SystemConfig,
	endpoints config.ServiceEndpoints,
) *config.SystemConfig {
	t.Helper()
	s := systemConfigWithEndpoint(systemConf, endpoints)
	s.ServiceTLS, _ = c.CredFactory.CreateServerCredentials(t, c.Config.TLSMode, endpoints.Server.Host)
	return s
}

// systemConfigWithEndpoint creates a new SystemConfig with a modified ServerEndpoint and MetricsEndpoint.
func systemConfigWithEndpoint(systemConf config.SystemConfig, e config.ServiceEndpoints) *config.SystemConfig {
	systemConf.ServiceEndpoints = e
	return &systemConf
}

func isMoreThanOneBitSet(bits int) bool {
	return bits != 0 && bits&(bits-1) != 0
}
