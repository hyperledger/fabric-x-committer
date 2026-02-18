/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"fmt"
	"testing"
	"time"

	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	commontypes "github.com/hyperledger/fabric-x-common/api/types"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/integration/runner"
	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/mock"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

const blockSize = 1

func newConfigTestEnv(t *testing.T, numHolders int) (
	*runner.CommitterRuntime, *mock.OrdererTestEnv, func(),
) {
	t.Helper()
	gomega.RegisterTestingT(t)

	c := runner.NewRuntime(t, &runner.Config{
		BlockSize:    100,
		BlockTimeout: 2 * time.Second,
		CrashTest:    true,
	})

	ordererServers := make([]*connection.ServerConfig, len(c.SystemConfig.Services.Orderer))
	for i, e := range c.SystemConfig.Services.Orderer {
		ordererServers[i] = &connection.ServerConfig{Endpoint: *e.GrpcEndpoint}
	}

	ordererEnv := mock.NewOrdererTestEnv(t, &mock.OrdererTestConfig{
		ChanID: "ch1",
		Config: &mock.OrdererConfig{
			ServerConfigs: ordererServers,
			BlockSize:     blockSize,
			// We want each block to contain exactly <blockSize> transactions.
			// Therefore, we set a higher block timeout so that we have enough time to send all the
			// transactions to the orderer and create a block.
			BlockTimeout:    5 * time.Minute,
			ConfigBlockPath: c.SystemConfig.ConfigBlockPath,
			SendConfigBlock: true,
		},
		NumHolders: numHolders,
	})

	c.Start(t, runner.CommitterTxPath)
	c.CreateNamespacesAndCommit(t, "1")

	sendTXs := func() {
		txs := make([][]*applicationpb.TxNamespace, blockSize)
		expected := make([]committerpb.Status, blockSize)
		for i := range blockSize {
			txs[i] = []*applicationpb.TxNamespace{{
				NsId:        "1",
				NsVersion:   0,
				BlindWrites: []*applicationpb.Write{{Key: []byte(fmt.Sprintf("key-%d", i))}},
			}}
			expected[i] = committerpb.Status_COMMITTED
		}
		c.MakeAndSendTransactionsToOrderer(t, txs, expected)
	}

	return c, ordererEnv, sendTXs
}

// TestConfigUpdateLifecyclePolicy verifies that the LifecycleEndorsement policy is
// correctly enforced after a config block update. It tests three independent dimensions:
//  1. Updated endorser + stale NsVersion → MVCC conflict (version gate)
//  2. Stale endorser + correct NsVersion → signature invalid (policy gate)
//  3. Updated endorser + correct NsVersion → committed (both gates pass)
func TestConfigUpdateLifecyclePolicy(t *testing.T) {
	t.Parallel()
	c, ordererEnv, sendTXs := newConfigTestEnv(t, 0)

	t.Log("Sanity check")
	sendTXs()

	// Meta-namespace uses LifecycleEndorsement (MSP-based) from the config block.
	metaTx, err := workload.CreateNamespacesTX(c.SystemConfig.Policy, 0, "2", "3")
	require.NoError(t, err)

	// TxBuilder adds valid MSP endorsements via TxEndorser.
	validTx := &applicationpb.Tx{
		Namespaces: metaTx.Namespaces,
	}
	c.SendTransactionsToOrderer(t,
		[]*servicepb.LoadGenTx{c.TxBuilder.MakeTx(validTx)},
		[]committerpb.Status{committerpb.Status_COMMITTED},
	)

	t.Log("Sanity check")
	sendTXs()

	// Submit a config block update with new crypto material (4 peer orgs)
	t.Log("Submit config block update")
	configCryptoPath := t.TempDir()
	// Bump PeerOrganizationCount from 2 to 4 so the LifecycleEndorsement policy
	// structurally changes. This proves the verifier loads the updated policy,
	// not just that NsVersion matches.
	configBlock, err := workload.CreateDefaultConfigBlockWithCrypto(configCryptoPath, &workload.ConfigBlock{
		ChannelID:             c.SystemConfig.Policy.ChannelID,
		OrdererEndpoints:      ordererEnv.AllRealOrdererEndpoints(),
		PeerOrganizationCount: 4,
	})
	require.NoError(t, err)
	err = ordererEnv.Orderer.SubmitBlock(t.Context(), configBlock)
	require.NoError(t, err)
	c.ValidateExpectedResultsInCommittedBlock(t, &runner.ExpectedStatusInBlock{
		Statuses: []committerpb.Status{committerpb.Status_COMMITTED},
	})

	// The config update changed the LifecycleEndorsement policy from 2 to 4 peer orgs
	// and bumped _config version from 0 to 1. We test three cases to verify that the
	// verifier and VC independently enforce the policy and version gates:
	//
	// We reuse namespaces "2" and "3" that were already created. Since the keys exist
	// in _meta at version 0, we set ReadWrite.Version so the VC treats these as
	// updates (not inserts, which would hit a unique-violation conflict).
	updatedEndorser, _ := workload.NewPolicyEndorserFromMsp(configCryptoPath)
	staleEndorser, _ := workload.NewPolicyEndorserFromMsp(c.SystemConfig.Policy.CryptoMaterialPath)

	setReadWriteVersions := func(tx *applicationpb.Tx, ver uint64) {
		for _, rw := range tx.Namespaces[0].ReadWrites {
			rw.Version = &ver
		}
	}

	// Case 1: Updated endorser (4-org) + stale NsVersion (0).
	// The endorser satisfies the new 4-org policy, but the stale NsVersion causes
	// an MVCC conflict against _config (now at version 1).
	t.Log("Case 1: updated endorser + stale config version → MVCC conflict")
	staleVersionMetaTx, err := workload.CreateNamespacesTX(c.SystemConfig.Policy, 0, "2", "3")
	require.NoError(t, err)
	setReadWriteVersions(staleVersionMetaTx, 0)
	staleVersionTx := &applicationpb.Tx{Namespaces: staleVersionMetaTx.Namespaces}
	updatedEndorsement, err := updatedEndorser.EndorseTxNs("stale-version-tx", staleVersionTx, 0)
	require.NoError(t, err)
	staleVersionTx.Endorsements = []*applicationpb.Endorsements{updatedEndorsement}
	c.SendTransactionsToOrderer(t,
		[]*servicepb.LoadGenTx{c.TxBuilder.MakeTxWithID("stale-version-tx", staleVersionTx)},
		[]committerpb.Status{committerpb.Status_ABORTED_MVCC_CONFLICT},
	)

	// Case 2: Stale endorser (2-org) + correct NsVersion (1).
	// The NsVersion matches _config, but the 2-org endorser no longer satisfies
	// the updated 4-org LifecycleEndorsement policy → signature invalid.
	t.Log("Case 2: stale endorser + current config version → signature invalid")
	staleEndorserMetaTx, err := workload.CreateNamespacesTX(c.SystemConfig.Policy, 1, "2", "3")
	require.NoError(t, err)
	setReadWriteVersions(staleEndorserMetaTx, 0)
	staleEndorserTx := &applicationpb.Tx{Namespaces: staleEndorserMetaTx.Namespaces}
	staleEndorserEndorsement, err := staleEndorser.EndorseTxNs("stale-endorser-tx", staleEndorserTx, 0)
	require.NoError(t, err)
	staleEndorserTx.Endorsements = []*applicationpb.Endorsements{staleEndorserEndorsement}
	c.SendTransactionsToOrderer(t,
		[]*servicepb.LoadGenTx{c.TxBuilder.MakeTxWithID("stale-endorser-tx", staleEndorserTx)},
		[]committerpb.Status{committerpb.Status_ABORTED_SIGNATURE_INVALID},
	)

	// Case 3: Updated endorser (4-org) + correct NsVersion (1).
	// Both the policy and version gates pass → committed.
	t.Log("Case 3: updated endorser + current config version → committed")
	freshMetaTx, err := workload.CreateNamespacesTX(c.SystemConfig.Policy, 1, "2", "3")
	require.NoError(t, err)
	setReadWriteVersions(freshMetaTx, 0)
	freshTx := &applicationpb.Tx{Namespaces: freshMetaTx.Namespaces}
	freshEndorsement, err := updatedEndorser.EndorseTxNs("fresh-meta-tx", freshTx, 0)
	require.NoError(t, err)
	freshTx.Endorsements = []*applicationpb.Endorsements{freshEndorsement}
	c.SendTransactionsToOrderer(t,
		[]*servicepb.LoadGenTx{c.TxBuilder.MakeTxWithID("fresh-meta-tx", freshTx)},
		[]committerpb.Status{committerpb.Status_COMMITTED},
	)
}

// TestConfigUpdateOrdererEndpoints verifies that a config block update causes the
// sidecar to switch to the new orderer endpoints, and that a subsequent config block
// update can switch back.
func TestConfigUpdateOrdererEndpoints(t *testing.T) {
	t.Parallel()
	c, ordererEnv, sendTXs := newConfigTestEnv(t, 1)

	t.Log("Sanity check")
	sendTXs()

	submitConfigBlock := func(endpoints []*commontypes.OrdererEndpoint) {
		ordererEnv.SubmitConfigBlock(t, &workload.ConfigBlock{
			ChannelID:             c.SystemConfig.Policy.ChannelID,
			OrdererEndpoints:      endpoints,
			PeerOrganizationCount: c.SystemConfig.Policy.PeerOrganizationCount,
		})
	}

	t.Log("Update the sidecar to use a holder orderer group")
	submitConfigBlock(ordererEnv.AllHolderEndpoints())
	c.ValidateExpectedResultsInCommittedBlock(t, &runner.ExpectedStatusInBlock{
		Statuses: []committerpb.Status{committerpb.Status_COMMITTED},
	})

	holdingBlock := c.LastReceivedBlockNumber + 2
	t.Logf("Holding block #%d", holdingBlock)
	ordererEnv.Holder.HoldFromBlock.Store(holdingBlock)

	t.Log("Restart sidecar to check that it restarts using the holding orderer")
	c.Sidecar.Restart(t)

	t.Log("Sanity check")
	sendTXs()

	t.Log("Submit new config block, and ensure it was not received")
	// We submit the config that returns to the non-holding orderer.
	// But it should not be processed as the sidecar should have switched to the holding
	// orderer.
	submitConfigBlock(ordererEnv.AllRealOrdererEndpoints())
	select {
	case <-c.CommittedBlock:
		t.Fatal("the sidecar cannot receive blocks since its orderer holds them")
	case <-time.After(30 * time.Second):
		t.Log("Fantastic")
	}

	t.Log("We expect the block to be held")
	nextBlock, err := c.CoordinatorClient.GetNextBlockNumberToCommit(t.Context(), nil)
	require.NoError(t, err)
	require.NotNil(t, nextBlock)
	require.Equal(t, holdingBlock, nextBlock.Number)

	t.Log("We advance the holder by one to allow the config block to pass through, but not other blocks")
	ordererEnv.Holder.HoldFromBlock.Add(1)
	c.ValidateExpectedResultsInCommittedBlock(t, &runner.ExpectedStatusInBlock{
		Statuses: []committerpb.Status{committerpb.Status_COMMITTED},
	})

	t.Log("The sidecar should use the non-holding orderer, so the holding should not affect the processing")
	sendTXs()
}

// TestConfigBlockImmediateCommit verifies that config blocks are committed immediately,
// bypassing the normal batching delays configured for the verifier and VC services.
func TestConfigBlockImmediateCommit(t *testing.T) {
	t.Parallel()
	gomega.RegisterTestingT(t)

	c := runner.NewRuntime(t, &runner.Config{
		BlockSize:                           100,
		BlockTimeout:                        5 * time.Minute,
		CrashTest:                           true,
		VCMinTransactionBatchSize:           100,
		VCTimeoutForMinTransactionBatchSize: 1 * time.Hour,
		VerifierBatchSizeCutoff:             100,
		VerifierBatchTimeCutoff:             1 * time.Hour,
	})

	ordererServers := make([]*connection.ServerConfig, len(c.SystemConfig.Services.Orderer))
	for i, e := range c.SystemConfig.Services.Orderer {
		ordererServers[i] = &connection.ServerConfig{Endpoint: *e.GrpcEndpoint}
	}
	ordererEnv := mock.NewOrdererTestEnv(t, &mock.OrdererTestConfig{
		ChanID: "ch1",
		Config: &mock.OrdererConfig{
			ServerConfigs:   ordererServers,
			BlockSize:       1, // Each block contains exactly 1 transaction.
			BlockTimeout:    5 * time.Minute,
			ConfigBlockPath: c.SystemConfig.ConfigBlockPath,
			SendConfigBlock: true,
		},
		NumHolders: 0,
	})

	// The Start function internally calls ensureAtLeastLastCommittedBlockNumber(t, 0)
	// which waits 2 minutes for block 0 to be committed. If config blocks weren't processed
	// immediately, this would timeout due to the 1-hour batching delays.
	t.Log("Starting services - block 0 (config block) should be committed immediately")
	startTime := time.Now()
	c.Start(t, runner.CommitterTxPath)
	elapsed := time.Since(startTime)
	// this time would be higher due to the start of all services and connection establishment.
	t.Logf("Services started and block 0 committed in %v", elapsed)

	submitConfigBlock := func() {
		ordererEnv.SubmitConfigBlock(t, &workload.ConfigBlock{
			ChannelID:             c.SystemConfig.Policy.ChannelID,
			OrdererEndpoints:      ordererEnv.AllRealOrdererEndpoints(),
			PeerOrganizationCount: c.SystemConfig.Policy.PeerOrganizationCount,
		})
	}

	t.Log("Submitting config block (block 1) - should be committed immediately")
	startTime = time.Now()

	submitConfigBlock()

	const maxWaitTime = 3 * time.Second
	select {
	case blk := <-c.CommittedBlock:
		elapsed = time.Since(startTime)
		t.Logf("Config block #%d committed in %v", blk.Header.Number, elapsed)
		require.Equal(t, uint64(1), blk.Header.Number)
		require.Less(t, elapsed, maxWaitTime)
	case <-time.After(maxWaitTime):
		t.Fatalf("Config block was not committed within %v", maxWaitTime)
	}

	t.Log("Config block was committed immediately, bypassing batching delays")
}
