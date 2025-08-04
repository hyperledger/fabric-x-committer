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

	_, e := sidecar.MalformedTxTestCases(t, &workload.TxBuilderFactory{})
	testSize := len(e)
	c := runner.NewRuntime(t, &runner.Config{
		NumVerifiers: 2,
		NumVCService: 2,
		BlockSize:    uint64(testSize),
		BlockTimeout: 1 * time.Second,
	})
	c.Start(t, runner.FullTxPath)
	c.CreateNamespacesAndCommit(t, "1")

	txs, expected := sidecar.MalformedTxTestCases(t, c.TxBuilder)
	c.SendTransactionsToOrderer(t, txs, expected)
}
