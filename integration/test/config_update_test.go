/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
	"github.com/hyperledger/fabric-x-committer/api/types"
	"github.com/hyperledger/fabric-x-committer/integration/runner"
	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/mock"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/signature"
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
		expected := &runner.ExpectedStatusInBlock{
			TxIDs:    make([]string, blockSize),
			Statuses: make([]protoblocktx.Status, blockSize),
		}
		txs := make([]*protoblocktx.Tx, blockSize)
		txIDPrefix := uuid.New().String()
		for i := range blockSize {
			expected.TxIDs[i] = txIDPrefix + strconv.Itoa(i)
			expected.Statuses[i] = protoblocktx.Status_COMMITTED
			txs[i] = &protoblocktx.Tx{
				Id: expected.TxIDs[i],
				Namespaces: []*protoblocktx.TxNamespace{{
					NsId:        "1",
					NsVersion:   0,
					BlindWrites: []*protoblocktx.Write{{Key: []byte(fmt.Sprintf("key-%d", i))}},
				}},
			}
			c.AddSignatures(t, txs[i])
		}
		c.SendTransactionsToOrderer(t, txs)
		c.ValidateExpectedResultsInCommittedBlock(t, expected)
	}

	t.Log("Sanity check")
	sendTXs()

	// We create the meta TX with the old signature.
	metaTX := c.CreateMetaTX(t, "2", "3")

	newMetaCrypto := c.UpdateCryptoForNs(t, types.MetaNamespaceID, signature.Ecdsa)
	submitConfigBlock := func(endpoints []*connection.OrdererEndpoint) {
		ordererEnv.SubmitConfigBlock(t, &workload.ConfigBlock{
			ChannelID:                    c.SystemConfig.ChannelID,
			OrdererEndpoints:             endpoints,
			MetaNamespaceVerificationKey: newMetaCrypto.PubKey,
		})
	}
	submitConfigBlock(ordererEnv.AllRealOrdererEndpoints())
	c.ValidateExpectedResultsInCommittedBlock(t, &runner.ExpectedStatusInBlock{
		Statuses: []protoblocktx.Status{protoblocktx.Status_COMMITTED},
	})

	c.SendTransactionsToOrderer(t, []*protoblocktx.Tx{metaTX})
	c.ValidateExpectedResultsInCommittedBlock(t, &runner.ExpectedStatusInBlock{
		TxIDs:    []string{metaTX.Id},
		Statuses: []protoblocktx.Status{protoblocktx.Status_ABORTED_SIGNATURE_INVALID},
	})

	// We replace the ID to prevent duplicated status, and re-sign it with the correct key.
	metaTX.Id = uuid.New().String()
	metaTX.Namespaces[0].NsVersion = 1
	c.AddSignatures(t, metaTX)
	c.SendTransactionsToOrderer(t, []*protoblocktx.Tx{metaTX})
	c.ValidateExpectedResultsInCommittedBlock(t, &runner.ExpectedStatusInBlock{
		TxIDs:    []string{metaTX.Id},
		Statuses: []protoblocktx.Status{protoblocktx.Status_COMMITTED},
	})

	t.Log("Sanity check")
	sendTXs()

	t.Log("Update the sidecar to use a holder orderer group")
	submitConfigBlock(ordererEnv.AllHolderEndpoints())
	c.ValidateExpectedResultsInCommittedBlock(t, &runner.ExpectedStatusInBlock{
		Statuses: []protoblocktx.Status{protoblocktx.Status_COMMITTED},
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
	lastCommittedBlock, err := c.CoordinatorClient.GetLastCommittedBlockNumber(t.Context(), nil)
	require.NoError(t, err)
	require.NotNil(t, lastCommittedBlock.Block)
	require.Equal(t, holdingBlock-1, lastCommittedBlock.Block.Number)

	t.Log("We advance the holder by one to allow the config block to pass through, but not other blocks")
	ordererEnv.Holder.HoldFromBlock.Add(1)
	c.ValidateExpectedResultsInCommittedBlock(t, &runner.ExpectedStatusInBlock{
		Statuses: []protoblocktx.Status{protoblocktx.Status_COMMITTED},
	})

	t.Log("The sidecar should use the non-holding orderer, so the holding should not affect the processing")
	sendTXs()
}
