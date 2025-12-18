/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"fmt"
	"testing"
	"time"

	commontypes "github.com/hyperledger/fabric-x-common/api/types"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/api/applicationpb"
	"github.com/hyperledger/fabric-x-committer/api/committerpb"
	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/integration/runner"
	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/mock"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

const blockSize = 1

func TestConfigUpdate(t *testing.T) {
	t.Parallel()
	gomega.RegisterTestingT(t)
	c := runner.NewRuntime(t, &runner.Config{
		NumVerifiers: 2,
		NumVCService: 2,
		BlockSize:    100,
		BlockTimeout: 2 * time.Second,
		CrashTest:    true,
	})
	ordererServers := make([]*connection.ServerConfig, len(c.SystemConfig.Endpoints.Orderer))
	for i, e := range c.SystemConfig.Endpoints.Orderer {
		ordererServers[i] = &connection.ServerConfig{Endpoint: *e.Server}
	}
	ordererEnv := mock.NewOrdererTestEnv(t, &mock.OrdererTestConfig{
		ChanID: "ch1",
		Config: &mock.OrdererConfig{
			ServerConfigs: ordererServers,
			NumService:    len(ordererServers),
			BlockSize:     blockSize,
			// We want each block to contain exactly <blockSize> transactions.
			// Therefore, we set a higher block timeout so that we have enough time to send all the
			// transactions to the orderer and create a block.
			BlockTimeout:    5 * time.Minute,
			ConfigBlockPath: c.SystemConfig.ConfigBlockPath,
			SendConfigBlock: true,
		},
		NumHolders: 1,
	})
	t.Log(c.SystemConfig.Endpoints.Orderer)
	t.Log(ordererEnv.AllRealOrdererEndpoints())

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

	t.Log("Sanity check")
	sendTXs()

	metaTx, err := workload.CreateNamespacesTX(c.SystemConfig.Policy, 1, "2", "3")
	require.NoError(t, err)

	// We sign the meta TX with the old signature.
	lgMetaTx := c.TxBuilder.MakeTx(metaTx)

	c.AddOrUpdateNamespaces(t, committerpb.MetaNamespaceID)
	verPolicies := c.TxBuilder.TxEndorser.VerificationPolicies()
	metaPolicy := verPolicies[committerpb.MetaNamespaceID]
	submitConfigBlock := func(endpoints []*commontypes.OrdererEndpoint) {
		ordererEnv.SubmitConfigBlock(t, &workload.ConfigBlock{
			ChannelID:                    c.SystemConfig.Policy.ChannelID,
			OrdererEndpoints:             endpoints,
			MetaNamespaceVerificationKey: metaPolicy.GetThresholdRule().GetPublicKey(),
		})
	}
	submitConfigBlock(ordererEnv.AllRealOrdererEndpoints())
	c.ValidateExpectedResultsInCommittedBlock(t, &runner.ExpectedStatusInBlock{
		Statuses: []committerpb.Status{committerpb.Status_COMMITTED},
	})

	// We send the old version and it fails.
	c.SendTransactionsToOrderer(
		t,
		[]*servicepb.LoadGenTx{lgMetaTx},
		[]committerpb.Status{committerpb.Status_ABORTED_SIGNATURE_INVALID},
	)

	// We send with the updated key and it works.
	c.MakeAndSendTransactionsToOrderer(
		t,
		[][]*applicationpb.TxNamespace{metaTx.Namespaces},
		[]committerpb.Status{committerpb.Status_COMMITTED},
	)

	t.Log("Sanity check")
	sendTXs()

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
