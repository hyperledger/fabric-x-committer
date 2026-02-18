/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"strings"
	"testing"
	"time"

	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/integration/runner"
)

var failureScenarios = [][]string{
	{"coordinator"},
	{"verifiers"},
	{"vcservices"},
	{"sidecar"},
	{"coordinator", "sidecar"},
	{"vcservices", "verifiers"},
	// We fail all four services, but we test the order too.
	// However, the order of vcservices and verifiers do not matter.
	{"coordinator", "vcservices", "verifiers", "sidecar"},
	{"verifiers", "vcservices", "coordinator", "sidecar"},
	{"sidecar", "vcservices", "verifiers", "coordinator"},
}

func TestCrashWhenIdle(t *testing.T) {
	t.Parallel()
	gomega.RegisterTestingT(t)
	c := runner.NewRuntime(t, &runner.Config{
		// We use a single TX per block to ensure we always get "all" the expected TXs in one block.
		BlockSize:    1,
		BlockTimeout: 2 * time.Second,
		CrashTest:    true,
	})
	c.Start(t, runner.FullTxPath)
	c.CreateNamespacesAndCommit(t, "1")

	// Scenario:
	// 1. Submit one transaction and verify its commitment.
	// 2. Terminate one or more services (excluding the ordering service).
	// 3. Resubmit the same transaction to the ordering service while some services remain offline.
	// 4. Restart the terminated services and confirm that all transactions are processed successfully,
	//    with the client receiving duplicate transaction ID statuses.
	txs := [][]*applicationpb.TxNamespace{{{
		BlindWrites: []*applicationpb.Write{{
			Key: []byte("k1"),
		}},
	}}}

	i := 1
	t.Logf("\n%d. Send transactions before stopping services and verify their commitment.\n", i)
	addSignAndSendTransactions(t, c, txs, []committerpb.Status{committerpb.Status_COMMITTED})

	for _, serviceNames := range failureScenarios {
		services := strings.Join(serviceNames, "-")
		i++
		t.Logf("\n%d. Stop "+services+"\n", i)
		stopServices(t, c, serviceNames)

		i++
		t.Logf("\n%d. Send transactions after stopping "+services+"\n", i)
		txIDs := addSignAndSendTransactions(t, c, txs, nil)

		time.Sleep(10 * time.Second) // allow time for the transactions to reach orderer and create a block

		i++
		t.Logf("\n%d. Restart "+services+"\n", i)
		restartServices(t, c, serviceNames)

		i++
		t.Logf("\n%d. Ensure that the last block is committed after restarting "+services+"\n", i)
		c.ValidateExpectedResultsInCommittedBlock(t, &runner.ExpectedStatusInBlock{
			TxIDs:    txIDs,
			Statuses: []committerpb.Status{committerpb.Status_COMMITTED},
		})
	}
}

func addSignAndSendTransactions(
	t *testing.T, c *runner.CommitterRuntime, txs [][]*applicationpb.TxNamespace, expectedStatus []committerpb.Status,
) []string {
	t.Helper()
	for _, tx := range txs {
		for _, ns := range tx {
			ns.NsId = "1"
			ns.NsVersion = 0
		}
	}
	return c.MakeAndSendTransactionsToOrderer(t, txs, expectedStatus)
}

func TestCrashWhenNonIdle(t *testing.T) {
	t.Parallel()
	gomega.RegisterTestingT(t)
	c := runner.NewRuntime(t, &runner.Config{
		BlockSize:         500,
		BlockTimeout:      120 * time.Second,
		LoadgenBlockLimit: 300,
		CrashTest:         true,
		// The limit of 300 with 500 transactions was chosen based on local/CI testing
		// to ensure that load is generated until all failure scenarios complete. There is
		// enough buffer here to ensure that the test would pass even on a faster server.
		// If we observe this limit to be problematic, we can determine a more appropriate limit:
		// run the load generator (loadgen) for a few seconds to observe the throughput,
		// use that throughput to calculate the required block limit and failure duration,
		// and then restart the runtime with the calculated limit. Even this approach may
		// not be accurate enough.
		// Note: In the code, we ensure new transactions are committed after executing all
		// failure scenarios, which should help to identify whether the current approach is adequate.
	})
	c.Start(t, runner.CommitterTxPathWithLoadGen)

	i := 0
	for _, serviceNames := range failureScenarios {
		services := strings.Join(serviceNames, "-")
		i++
		t.Logf("\n ==== %d. Stop "+services+" ==== \n ", i)
		stopServices(t, c, serviceNames)

		i++
		t.Logf("\n ==== %d. Restart "+services+" ==== \n", i)
		restartServices(t, c, serviceNames)
	}

	// After all failure scenarios are executed, we should ensure that
	// a few more transactions are being committed.
	count := c.CountStatus(t, committerpb.Status_COMMITTED)
	require.Eventually(t, func() bool {
		return c.CountStatus(t, committerpb.Status_COMMITTED) > count
	}, 30*time.Second, 1*time.Millisecond)

	// 1. Block 0 holds the config transaction.
	// 2. Block 1 holds namespace creation transaction.
	actualCountedBlocks := c.SystemConfig.LoadGenBlockLimit - 1  // block 0 is not counted in the limit
	totalTxs := c.SystemConfig.BlockSize*actualCountedBlocks + 2 // we add 2 txs (config tx and namespace tx).
	require.Eventually(t, func() bool {
		count := c.CountStatus(t, committerpb.Status_COMMITTED)
		t.Logf("count: %d, expected: %d", count, totalTxs)
		return count == int(totalTxs) //nolint:gosec
	}, 300*time.Second, 1*time.Second)
}

func stopServices(t *testing.T, c *runner.CommitterRuntime, serviceNames []string) {
	t.Helper()
	for _, s := range serviceNames {
		switch s {
		case "coordinator":
			c.Coordinator.Stop(t)
		case "verifiers":
			for _, verifier := range c.Verifier {
				verifier.Stop(t)
			}
		case "vcservices":
			for _, vcservice := range c.VcService {
				vcservice.Stop(t)
			}
		case "sidecar":
			c.Sidecar.Stop(t)
		}
		time.Sleep(2 * time.Second) // between stop, wait for a few seconds.
	}
}

func restartServices(t *testing.T, c *runner.CommitterRuntime, serviceNames []string) {
	t.Helper()
	for _, s := range serviceNames {
		switch s {
		case "coordinator":
			c.Coordinator.Restart(t)
		case "verifiers":
			for _, verifier := range c.Verifier {
				verifier.Restart(t)
			}
		case "vcservices":
			for _, vcservice := range c.VcService {
				vcservice.Restart(t)
			}
		case "sidecar":
			c.Sidecar.Restart(t)
		}
		time.Sleep(2 * time.Second) // between restart, wait for a few seconds.
	}
}
