/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"context"
	"testing"
	"time"

	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	"github.com/hyperledger/fabric-x-committer/integration/runner"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/grpcerror"
	"github.com/hyperledger/fabric-x-committer/utils/test"
	"github.com/hyperledger/fabric-x-committer/utils/testcrypto"
)

// TestDynamicTLS verifies that the sidecar and query service dynamically update
// their trusted TLS CA pool when config blocks add or remove peer organizations,
// while preserving static (YAML-configured) root CAs.
func TestDynamicTLS(t *testing.T) {
	t.Parallel()

	c := runner.NewRuntime(t, &runner.Config{
		TLSMode:                 connection.MutualTLSMode,
		PeerOrganizationCount:   3,
		BlockTimeout:            2 * time.Second,
		QueryTLSRefreshInterval: 5 * time.Second,
		CrashTest:               true,
	})
	// Start orderer servers in-process so SubmitConfigBlock can write directly
	// to the orderer's block channel (separate-process orderers don't share memory).
	c.OrdererEnv.StartServers(t)
	c.Start(t, runner.CommitterTxPath|runner.QueryService)

	serverCACertPaths := c.SystemConfig.ClientTLS.CACertPaths
	sidecarEndpoint := c.SystemConfig.Services.Sidecar.GrpcEndpoint
	queryEndpoint := c.SystemConfig.Services.Query.GrpcEndpoint

	// Per-org mTLS configs. Crypto artifacts don't change across config block updates,
	// so these remain valid throughout the test.
	orgTLS := [3]connection.TLSConfig{}
	for i := range orgTLS {
		orgTLS[i] = test.OrgClientTLSConfig(c.OrdererEnv.ArtifactsPath, i, serverCACertPaths)
	}

	// Step 1: Assert all three peer orgs can connect to sidecar and query service.
	// The sidecar updates dynamic TLS immediately when processing the genesis config block.
	// The query service polls the DB periodically, so we use Eventually for it.
	t.Log("Step 1: Initial connection - all three orgs should connect")
	for orgIdx, tlsCfg := range orgTLS {
		require.NoError(t, tryRPC(sidecarEndpoint, tlsCfg), "sidecar: peer-org-%d should connect", orgIdx)
		require.EventuallyWithT(t, func(ct *assert.CollectT) {
			require.NoError(ct, tryRPC(queryEndpoint, tlsCfg), "query: peer-org-%d should connect", orgIdx)
		}, 15*time.Second, time.Second)
	}

	// Step 2: Submit config block removing peer-org-2 (keep only peer-org-0 and peer-org-1).
	// CreateOrExtendConfigBlockWithCrypto retains existing crypto on disk, so peer-org-2's
	// certs remain available for reconnection in Step 4.
	t.Log("Step 2: Dynamic removal - submit config with 2 peer orgs")
	c.OrdererEnv.SubmitConfigBlock(t, &testcrypto.ConfigBlock{
		OrdererEndpoints:      c.OrdererEnv.AllEndpoints,
		PeerOrganizationCount: 2,
	})
	c.ValidateExpectedResultsInCommittedBlock(t, &runner.ExpectedStatusInBlock{
		Statuses: []committerpb.Status{committerpb.Status_COMMITTED},
	})

	// Step 3: Verify peer-org-2 is rejected and peer-org-0 remains trusted.
	t.Log("Step 3: Negative assertion - peer-org-2 rejected, peer-org-0 accepted")

	// Sidecar updates immediately from config block processing.
	require.NoError(t, tryRPC(sidecarEndpoint, orgTLS[0]), "sidecar: peer-org-0 should connect")
	require.Error(t, tryRPC(sidecarEndpoint, orgTLS[2]), "sidecar: peer-org-2 should be rejected")

	// Query service polls the DB; wait for the TLS refresh (up to 15s).
	require.NoError(t, tryRPC(queryEndpoint, orgTLS[0]), "query: peer-org-0 should connect")
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		require.Error(ct, tryRPC(queryEndpoint, orgTLS[2]), "query service should reject peer-org-2 after TLS refresh")
	}, 15*time.Second, time.Second)

	// Step 4: Restore peer-org-2.
	t.Log("Step 4: Restoration - add peer-org-2 back")
	c.OrdererEnv.SubmitConfigBlock(t, &testcrypto.ConfigBlock{
		OrdererEndpoints:      c.OrdererEnv.AllEndpoints,
		PeerOrganizationCount: 3,
	})
	c.ValidateExpectedResultsInCommittedBlock(t, &runner.ExpectedStatusInBlock{
		Statuses: []committerpb.Status{committerpb.Status_COMMITTED},
	})

	// Sidecar: peer-org-2 connects immediately.
	require.NoError(t, tryRPC(sidecarEndpoint, orgTLS[2]), "sidecar: peer-org-2 should connect")

	// Query service: wait for TLS refresh.
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		require.NoError(ct, tryRPC(queryEndpoint, orgTLS[2]), "query: peer-org-2 should connect")
	}, 15*time.Second, time.Second)

	// Step 5: Static persistence - CredentialsFactory client still works.
	t.Log("Step 5: Static persistence - static TLS client still trusted")
	require.NoError(t, tryRPC(sidecarEndpoint, c.SystemConfig.ClientTLS), "sidecar (static) should connect")
	require.NoError(t, tryRPC(queryEndpoint, c.SystemConfig.ClientTLS), "query (static) should connect")

	// Step 6: Restart sidecar and query service, then verify all clients can reconnect.
	// This ensures that TLS initialization from persisted config blocks works correctly
	// and that the static TLS config is still honored after restart.
	t.Log("Step 6: Service restart - all clients should reconnect with persisted TLS config")

	// Stop and restart sidecar and query service.
	// The restart reuses the same config, so endpoints don't change.
	c.Sidecar.Restart(t)
	c.QueryService.Restart(t)

	// Brief pause to allow old sockets to clear TIME_WAIT; prevents bind failure.
	time.Sleep(2 * time.Second)

	// Wait for services to be ready. Poll frequently to minimize latency.
	t.Log("Waiting for services to be ready after restart...")
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		require.NoError(ct, tryRPC(sidecarEndpoint, orgTLS[0]), "sidecar should be ready after restart")
	}, 60*time.Second, 100*time.Millisecond)

	// All orgs should now connect (sidecar TLS was loaded from persisted config during startup).
	for orgIdx, tlsCfg := range orgTLS {
		require.NoError(t, tryRPC(sidecarEndpoint, tlsCfg), "sidecar after restart: peer-org-%d should connect", orgIdx)
		require.EventuallyWithT(t, func(ct *assert.CollectT) {
			require.NoError(ct, tryRPC(queryEndpoint, tlsCfg),
				"query after restart: peer-org-%d should connect", orgIdx)
		}, 15*time.Second, time.Second)
	}

	// Static TLS client should still work after restart.
	require.NoError(t, tryRPC(sidecarEndpoint, c.SystemConfig.ClientTLS),
		"sidecar (static) after restart should connect")
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		require.NoError(ct, tryRPC(queryEndpoint, c.SystemConfig.ClientTLS),
			"query (static) after restart should connect")
	}, 15*time.Second, time.Second)
}

// tryRPC attempts a lightweight gRPC call and returns an error only if the TLS
// handshake fails. Application-level gRPC errors (InvalidArgument, Unimplemented,
// etc.) indicate a successful TLS connection and are treated as success.
func tryRPC(endpoint connection.WithAddress, tlsConfig connection.TLSConfig) error {
	creds, err := tlsConfig.ClientCredentials()
	if err != nil {
		return err
	}
	conn, err := grpc.NewClient(endpoint.Address(), grpc.WithTransportCredentials(creds))
	if err != nil {
		return err
	}
	defer conn.Close() //nolint:errcheck

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client := committerpb.NewQueryServiceClient(conn)
	_, err = client.GetTransactionStatus(ctx, &committerpb.TxStatusQuery{})
	// FilterUnavailableErrorCode returns nil for transient connectivity errors
	// (Unavailable, DeadlineExceeded) and passes through application-level errors.
	// An application-level error means TLS succeeded, so we invert: if the filter
	// passes through an error, the connection worked.
	if grpcerror.FilterUnavailableErrorCode(err) != nil {
		return nil
	}
	return err
}
