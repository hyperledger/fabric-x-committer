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
	"github.com/hyperledger/fabric-x-common/internaltools/configtxgen"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
	"github.com/hyperledger/fabric-x-committer/api/protocoordinatorservice"
	"github.com/hyperledger/fabric-x-committer/api/protoloadgen"
	"github.com/hyperledger/fabric-x-committer/api/protonotify"
	"github.com/hyperledger/fabric-x-committer/api/protoqueryservice"
	"github.com/hyperledger/fabric-x-committer/api/types"
	"github.com/hyperledger/fabric-x-committer/cmd/config"
	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/service/sidecar"
	"github.com/hyperledger/fabric-x-committer/service/sidecar/sidecarclient"
	"github.com/hyperledger/fabric-x-committer/service/vc"
	"github.com/hyperledger/fabric-x-committer/service/vc/dbtest"
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

		dbEnv *vc.DatabaseTestEnv

		ordererStream      *test.BroadcastStream
		CoordinatorClient  protocoordinatorservice.CoordinatorClient
		QueryServiceClient protoqueryservice.QueryServiceClient
		sidecarClient      *sidecarclient.Client
		notifyClient       protonotify.NotifierClient
		notifyStream       protonotify.Notifier_OpenNotificationStreamClient

		CommittedBlock chan *common.Block

		TxBuilder *workload.TxBuilder

		config *Config

		seedForCryptoGen *rand.Rand

		LastReceivedBlockNumber uint64

		CredFactory *test.CredentialsFactory
	}

	// Crypto holds crypto material for a namespace.
	Crypto struct {
		Namespace  string
		Profile    *workload.Policy
		HashSigner *workload.HashSignerVerifier
		NsSigner   *sigtest.NsSigner
		PubKey     []byte
		PubKeyPath string
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
		config: conf,
		SystemConfig: config.SystemConfig{
			BlockSize:         conf.BlockSize,
			BlockTimeout:      conf.BlockTimeout,
			LoadGenBlockLimit: conf.LoadgenBlockLimit,
			LoadGenWorkers:    1,
			Policy: &workload.PolicyProfile{
				ChannelID:         TestChannelName,
				NamespacePolicies: make(map[string]*workload.Policy),
			},
			Logging: &logging.DefaultConfig,
		},
		CommittedBlock:   make(chan *common.Block, 100),
		seedForCryptoGen: rand.New(rand.NewSource(10)),
	}
	c.AddOrUpdateNamespaces(t, types.MetaNamespaceID, workload.GeneratedNamespaceID, "1", "2", "3")

	t.Log("Making DB env")
	if conf.DBConnection == nil {
		c.dbEnv = vc.NewDatabaseTestEnv(t)
	} else {
		c.dbEnv = vc.NewDatabaseTestEnvWithCustomConnection(t, conf.DBConnection)
	}

	s := &c.SystemConfig
	s.DB.Name = c.dbEnv.DBConf.Database
	s.DB.Password = c.dbEnv.DBConf.Password
	s.DB.LoadBalance = c.dbEnv.DBConf.LoadBalance
	s.DB.Endpoints = c.dbEnv.DBConf.Endpoints
	s.DB.TLS = c.dbEnv.DBConf.TLS
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
	s.Policy.OrdererEndpoints = make([]*ordererconn.Endpoint, len(s.Endpoints.Orderer))
	for i, e := range s.Endpoints.Orderer {
		s.Policy.OrdererEndpoints[i] = &ordererconn.Endpoint{MspID: "org", Endpoint: *e.Server}
	}

	test.LogStruct(t, "System Parameters", s)

	t.Log("Creating config block")
	configBlock, err := workload.CreateConfigBlock(s.Policy)
	require.NoError(t, err)
	err = configtxgen.WriteOutputBlock(configBlock, s.ConfigBlockPath)
	require.NoError(t, err)

	t.Log("create TLS manager and clients certificate")
	c.CredFactory = test.NewCredentialsFactory(t)
	s.ClientTLS, _ = c.CredFactory.CreateClientCredentials(t, c.config.TLSMode)

	t.Log("Create processes")
	c.MockOrderer = newProcess(t, cmdOrderer, s.WithEndpoint(s.Endpoints.Orderer[0]))
	for i, e := range s.Endpoints.Verifier {
		p := cmdVerifier
		p.Name = fmt.Sprintf("%s-%d", p.Name, i)
		// we generate different keys for each verifier.
		c.Verifier = append(c.Verifier, newProcess(t, p, c.createSystemConfigWithServerTLS(t, e)))
	}

	for i, e := range s.Endpoints.VCService {
		p := cmdVC
		p.Name = fmt.Sprintf("%s-%d", p.Name, i)
		// we generate different keys for each vc-service.
		c.VcService = append(c.VcService, newProcess(t, p, c.createSystemConfigWithServerTLS(t, e)))
	}

	c.Coordinator = newProcess(t, cmdCoordinator, c.createSystemConfigWithServerTLS(t, s.Endpoints.Coordinator))

	c.QueryService = newProcess(t, cmdQuery, c.createSystemConfigWithServerTLS(t, s.Endpoints.Query))

	c.Sidecar = newProcess(t, cmdSidecar, c.createSystemConfigWithServerTLS(t, s.Endpoints.Sidecar))

	t.Log("Create clients")
	c.CoordinatorClient = protocoordinatorservice.NewCoordinatorClient(
		test.NewSecuredConnection(t, s.Endpoints.Coordinator.Server, c.SystemConfig.ClientTLS),
	)

	c.QueryServiceClient = protoqueryservice.NewQueryServiceClient(
		test.NewSecuredConnection(t, s.Endpoints.Query.Server, c.SystemConfig.ClientTLS),
	)

	c.notifyClient = protonotify.NewNotifierClient(
		test.NewSecuredConnection(t, s.Endpoints.Sidecar.Server, c.SystemConfig.ClientTLS),
	)

	c.ordererStream, err = test.NewBroadcastStream(t.Context(), &ordererconn.Config{
		Connection: ordererconn.ConnectionConfig{
			Endpoints: s.Policy.OrdererEndpoints,
		},
		ChannelID:     s.Policy.ChannelID,
		Identity:      s.Policy.Identity,
		ConsensusType: ordererconn.Bft,
	})
	require.NoError(t, err)
	t.Cleanup(c.ordererStream.CloseConnections)

	c.sidecarClient, err = sidecarclient.New(&sidecarclient.Parameters{
		ChannelID: s.Policy.ChannelID,
		Client:    test.NewTLSClientConfig(s.ClientTLS, s.Endpoints.Sidecar.Server),
	})
	require.NoError(t, err)
	t.Cleanup(c.sidecarClient.CloseConnections)
	return c
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
		var err error
		c.notifyStream, err = c.notifyClient.OpenNotificationStream(t.Context())
		require.NoError(t, err)
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
	newProcess(t, loadGenParams, c.createSystemConfigWithServerTLS(t, s.Endpoints.LoadGen)).Restart(t)
	if isDist {
		s.LoadGenWorkers = 1
		loadGenParams.Name = "dist-loadgen"
		loadGenParams.Template = config.TemplateLoadGenDistributedLoadGenClient
		newProcess(t, loadGenParams, s.WithEndpoint(config.ServiceEndpoints{})).Restart(t)
	}
}

func (c *CommitterRuntime) startBlockDelivery(t *testing.T) {
	t.Helper()
	t.Log("Running delivery client")
	test.RunServiceForTest(t.Context(), t, func(ctx context.Context) error {
		return connection.FilterStreamRPCError(c.sidecarClient.Deliver(ctx, &sidecarclient.DeliverParameters{
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
			Seed:   c.seedForCryptoGen.Int63(),
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
		[][]*protoblocktx.TxNamespace{metaTX.Namespaces},
		[]protoblocktx.Status{protoblocktx.Status_COMMITTED},
	)
}

// MakeAndSendTransactionsToOrderer creates a block with given transactions, send it to the committer,
// and verify the result.
func (c *CommitterRuntime) MakeAndSendTransactionsToOrderer(
	t *testing.T, txsNs [][]*protoblocktx.TxNamespace, expectedStatus []protoblocktx.Status,
) []string {
	t.Helper()
	txs := make([]*protoloadgen.TX, len(txsNs))

	for i, namespaces := range txsNs {
		tx := &protoblocktx.Tx{
			Namespaces: namespaces,
		}
		if expectedStatus != nil && expectedStatus[i] == protoblocktx.Status_ABORTED_SIGNATURE_INVALID {
			tx.Signatures = make([][]byte, len(namespaces))
			for nsIdx := range namespaces {
				tx.Signatures[nsIdx] = []byte("dummy")
			}
		}
		txs[i] = c.TxBuilder.MakeTx(tx)
	}

	return c.SendTransactionsToOrderer(t, txs, expectedStatus)
}

// SendTransactionsToOrderer creates a block with given transactions, send it to the committer, and verify the result.
func (c *CommitterRuntime) SendTransactionsToOrderer(
	t *testing.T, txs []*protoloadgen.TX, expectedStatus []protoblocktx.Status,
) []string {
	t.Helper()
	expected := &ExpectedStatusInBlock{
		Statuses: expectedStatus,
		TxIDs:    make([]string, len(txs)),
	}
	for i, tx := range txs {
		expected.TxIDs[i] = tx.Id
	}

	if !c.config.CrashTest {
		err := c.notifyStream.Send(&protonotify.NotificationRequest{
			TxStatusRequest: &protonotify.TxStatusRequest{
				TxIds: expected.TxIDs,
			},
			Timeout: durationpb.New(3 * time.Minute),
		})
		require.NoError(t, err)
		// Allows processing the request before submitting the payload.
		time.Sleep(1 * time.Second)
	}

	err := c.ordererStream.SendBatch(workload.MapToEnvelopeBatch(0, txs))
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
	Statuses []protoblocktx.Status
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
		actualStatuses[i] = protoblocktx.Status(sB).String()
	}
	require.Equal(t, expectedStatuses, actualStatuses)

	c.ensureLastCommittedBlockNumber(t, blk.Header.Number)

	var persistedTxIDs []string
	persistedTxIDsStatus := make(map[string]*protoblocktx.StatusWithHeight)
	nonDuplicateTxIDsStatus := make(map[string]*protoblocktx.StatusWithHeight)
	duplicateTxIDsStatus := make(map[string]*protoblocktx.StatusWithHeight)
	for i, tID := range expected.TxIDs {
		//nolint:gosec // int -> uint32.
		s := types.NewStatusWithHeight(expected.Statuses[i], blk.Header.Number, uint32(i))
		if s.Code == protoblocktx.Status_REJECTED_DUPLICATE_TX_ID {
			duplicateTxIDsStatus[tID] = s
		} else {
			nonDuplicateTxIDsStatus[tID] = s
		}

		if sidecar.IsStatusStoredInDB(s.Code) {
			persistedTxIDsStatus[tID] = s
			persistedTxIDs = append(persistedTxIDs, tID)
		}
	}

	c.dbEnv.StatusExistsForNonDuplicateTxID(t, persistedTxIDsStatus)
	// For the duplicate txID, neither the status nor the height would match the entry in the
	// transaction status table.
	c.dbEnv.StatusExistsWithDifferentHeightForDuplicateTxID(t, duplicateTxIDsStatus)

	ctx, cancel := context.WithTimeout(t.Context(), 1*time.Minute)
	defer cancel()
	test.EnsurePersistedTxStatus(ctx, t, c.CoordinatorClient, persistedTxIDs, persistedTxIDsStatus)

	if len(expected.TxIDs) == 0 || c.config.CrashTest {
		return
	}

	sidecar.RequireNotifications(t, c.notifyStream, blk.Header.Number, expected.TxIDs, expected.Statuses)
}

// CountStatus returns the number of transactions with a given tx status.
func (c *CommitterRuntime) CountStatus(t *testing.T, status protoblocktx.Status) int {
	t.Helper()
	return c.dbEnv.CountStatus(t, status)
}

// CountAlternateStatus returns the number of transactions not with a given tx status.
func (c *CommitterRuntime) CountAlternateStatus(t *testing.T, status protoblocktx.Status) int {
	t.Helper()
	return c.dbEnv.CountAlternateStatus(t, status)
}

func (c *CommitterRuntime) ensureLastCommittedBlockNumber(t *testing.T, blkNum uint64) {
	t.Helper()
	c.ensureAtLeastLastCommittedBlockNumber(t, blkNum)

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	lastCommittedBlock, err := c.CoordinatorClient.GetLastCommittedBlockNumber(ctx, nil)
	require.NoError(t, err)
	require.NotNil(t, lastCommittedBlock.Block)
	require.Equal(t, blkNum, lastCommittedBlock.Block.Number)
}

func (c *CommitterRuntime) ensureAtLeastLastCommittedBlockNumber(t *testing.T, blkNum uint64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		lastCommittedBlock, err := c.CoordinatorClient.GetLastCommittedBlockNumber(ctx, nil)
		require.NoError(ct, err)
		require.NotNil(ct, lastCommittedBlock.Block)
		require.GreaterOrEqual(ct, lastCommittedBlock.Block.Number, blkNum)
	}, 2*time.Minute, 250*time.Millisecond)
}

func (c *CommitterRuntime) createSystemConfigWithServerTLS(
	t *testing.T,
	endpoints config.ServiceEndpoints,
) *config.SystemConfig {
	t.Helper()
	serviceCfg := c.SystemConfig
	serviceCfg.ServiceTLS, _ = c.CredFactory.CreateServerCredentials(t, c.config.TLSMode, endpoints.Server.Host)
	serviceCfg.ServiceEndpoints = endpoints
	return &serviceCfg
}

func isMoreThanOneBitSet(bits int) bool {
	return bits != 0 && bits&(bits-1) != 0
}
