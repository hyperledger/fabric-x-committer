/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"testing"
	"time"

	"github.com/onsi/gomega"

	"github.com/hyperledger/fabric-x-committer/integration/runner"
	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/service/sidecar"
)

func TestBadlyFormedTxs(t *testing.T) {
	t.Parallel()
	gomega.RegisterTestingT(t)

	// We pre-build the test cases to get the test size, which we use as the block size.
	_, e := sidecar.MalformedTxTestCases(&workload.TxBuilder{})
	c := runner.NewRuntime(t, &runner.Config{
		NumVerifiers: 2,
		NumVCService: 2,
		BlockSize:    uint64(len(e)),
		BlockTimeout: 1 * time.Second,
	})
	c.Start(t, runner.FullTxPath)
	c.CreateNamespacesAndCommit(t, "1")

	// We re-build the tests cases with a proper builder.
	txs, expected := sidecar.MalformedTxTestCases(c.TxBuilder)
	c.SendTransactionsToOrderer(t, txs, expected)
}
