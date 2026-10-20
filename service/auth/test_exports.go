/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package auth

import (
	"path"
	"testing"
	"time"

	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/hyperledger/fabric-x-common/protoutil"
	"github.com/hyperledger/fabric-x-common/tools/cryptogen"
	"github.com/hyperledger/fabric-x-common/utils/testcrypto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/utils/acl"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/serve"
	"github.com/hyperledger/fabric-x-committer/utils/statedb"
	"github.com/hyperledger/fabric-x-committer/utils/test"
	"github.com/hyperledger/fabric-x-committer/utils/testdb"
)

// defaultTestChannelID is the channel NewAuthTestEnv builds a configuration for when it creates one.
const defaultTestChannelID = "mychannel"

type (
	// TestEnv is an Auth service test environment.
	TestEnv struct {
		*acl.IssueParams
		Config *connection.MultiClientConfig
	}

	// TestEnvParams describes the AuthService NewAuthTestEnv should start.
	TestEnvParams struct {
		ServerTLS     connection.TLSConfig
		ClientTLS     connection.TLSConfig
		TokenTTL      time.Duration
		ArtifactsPath string
		// Instances is how many AuthServices to start over the one shared database. Zero means one.
		Instances int
		// ChallengeRateLimit throttles IssueNonce and Authenticate. Zero disables it.
		ChallengeRateLimit serve.RateLimitConfig
	}
)

// NewAuthTestEnv starts AuthService instances over one shared database, seeded with the channel
// configuration found in (or created under) the artifacts path, and returns once they authenticate.
func NewAuthTestEnv(t *testing.T, params *TestEnvParams) *TestEnv {
	t.Helper()
	if params == nil {
		params = &TestEnvParams{}
	}
	if params.TokenTTL <= 0 {
		params.TokenTTL = 30 * time.Minute
	}
	if params.ArtifactsPath == "" {
		params.ArtifactsPath = t.TempDir()
	}

	configBlock, readErr := protoutil.ReadBlockFromFile(path.Join(params.ArtifactsPath, cryptogen.ConfigBlockFileName))
	if readErr != nil {
		var err error
		configBlock, err = testcrypto.CreateOrExtendConfigBlockWithCrypto(params.ArtifactsPath, &testcrypto.ConfigBlock{
			ChannelID:             defaultTestChannelID,
			PeerOrganizationCount: 1,
		})
		require.NoError(t, err)
	}
	// The block is the authority on its own channel, so the env cannot be pointed at one channel's
	// configuration while it signs envelopes for another.
	channelID, err := protoutil.GetChannelIDFromBlock(configBlock)
	require.NoError(t, err)

	dbConf := prepareTestDatabase(t)
	seedConfigTransaction(t, dbConf, configBlock)

	endpoints := make([]*connection.Endpoint, max(params.Instances, 1))
	for i := range endpoints {
		serverConfig := test.NewLocalHostServiceConfig(params.ServerTLS)
		service, err := NewAuthService(&Config{
			Database:                dbConf,
			TokenTTL:                params.TokenTTL,
			NonceTTL:                5 * time.Minute,
			EnvelopeFreshnessWindow: 5 * time.Minute,
			ConfigRefreshInterval:   100 * time.Millisecond,
			SweepInterval:           time.Minute,
			ChallengeRateLimit:      params.ChallengeRateLimit,
		})
		require.NoError(t, err)
		test.RunServiceAndServeForTest(t.Context(), t, service, serverConfig)
		endpoints[i] = &serverConfig.GRPC.Endpoint
	}

	env := NewAuthClientTestEnv(
		t, test.NewTLSMultiClientConfig(params.ClientTLS, endpoints...), params.ArtifactsPath, channelID,
	)
	env.IssueToken(t)
	return env
}

// NewAuthClientTestEnv connects to running AuthService instances and
// signs as the first peer identity found under artifactsPath. The client balances across the instances,
// so a nonce issued by one of them is redeemed at another.
func NewAuthClientTestEnv(
	t *testing.T, config *connection.MultiClientConfig, artifactsPath, channelID string,
) *TestEnv {
	t.Helper()
	identities, err := testcrypto.GetPeersIdentities(artifactsPath)
	require.NoError(t, err)
	require.NotEmpty(t, identities, "no signing identity under %s", artifactsPath)

	certHash, err := acl.TLSCertHash(config.TLS)
	require.NoError(t, err)

	conn, err := connection.NewLoadBalancedConnection(config)
	require.NoError(t, err)
	t.Cleanup(func() { connection.CloseConnectionsLog(conn) })

	return &TestEnv{
		IssueParams: &acl.IssueParams{
			Client:      servicepb.NewAuthServiceClient(conn),
			Signer:      identities[0],
			ChannelID:   channelID,
			TLSCertHash: certHash,
		},
		Config: config,
	}
}

// IssueToken authenticates and returns a token the resource servers accept. It retries until the service
// has loaded a channel configuration to authenticate against.
func (e *TestEnv) IssueToken(t *testing.T) string {
	t.Helper()
	var token string
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		issued, err := acl.IssueToken(t.Context(), e.IssueParams)
		require.NoError(ct, err)
		token = issued.GetToken()
	}, 2*time.Minute, 100*time.Millisecond, "the auth service never issued a token")
	return token
}

// prepareTestDatabase provisions a test database with the system tables, which include the auth tables.
func prepareTestDatabase(t *testing.T) *statedb.Config {
	t.Helper()
	cs := testdb.PrepareTestEnv(t)
	dbConf := &statedb.Config{
		Endpoints:      cs.Endpoints,
		Username:       cs.User,
		Password:       cs.Password,
		Database:       cs.Database,
		MaxConnections: 10,
		MinConnections: 1,
		TLS:            cs.TLS,
		Retry:          testdb.DefaultRetry,
	}
	require.NoError(t, statedb.SetupSystemTablesAndNamespaces(t.Context(), dbConf))
	return dbConf
}

// seedConfigTransaction writes the block's configuration envelope where the service reads it, standing in
// for the committer that would otherwise have to commit it first.
func seedConfigTransaction(t *testing.T, dbConf *statedb.Config, block *common.Block) {
	t.Helper()
	envelope, err := protoutil.ExtractEnvelope(block, 0)
	require.NoError(t, err)
	envelopeBytes, err := proto.Marshal(envelope)
	require.NoError(t, err)

	pool, err := statedb.NewPool(t.Context(), dbConf)
	require.NoError(t, err)
	defer pool.Close()
	_, err = pool.Exec(t.Context(),
		"INSERT INTO "+statedb.TableName(committerpb.ConfigNamespaceID)+" (key, value, version) VALUES ($1, $2, 0)",
		[]byte(committerpb.ConfigKey), envelopeBytes)
	require.NoError(t, err)
}
