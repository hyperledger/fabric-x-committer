/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"testing"
	"time"

	"github.com/onsi/gomega"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
	"github.com/hyperledger/fabric-x-committer/integration/runner"
	"github.com/hyperledger/fabric-x-committer/service/sidecar"
)

func TestBadlyFormedTxs(t *testing.T) {
	t.Parallel()
	gomega.RegisterTestingT(t)

	testSize := len(sidecar.MalformedTxTestCases)
	c := runner.NewRuntime(t, &runner.Config{
		NumVerifiers: 2,
		NumVCService: 2,
		BlockSize:    uint64(testSize),
		BlockTimeout: 1 * time.Second,
	})
	c.Start(t, runner.FullTxPath)
	c.CreateNamespacesAndCommit(t, "1")

	txs := make([]*protoblocktx.Tx, testSize)
	expected := &runner.ExpectedStatusInBlock{
		TxIDs:    make([]string, testSize),
		Statuses: make([]protoblocktx.Status, testSize),
	}
	for i, tt := range sidecar.MalformedTxTestCases {
		txs[i] = tt.Tx
		expected.TxIDs[i] = tt.Tx.Id
		if tt.ExpectedStatus != protoblocktx.Status_NOT_VALIDATED {
			expected.Statuses[i] = tt.ExpectedStatus
		} else {
			// We use invalid signature as the default because the TXs are not properly signed.
			expected.Statuses[i] = protoblocktx.Status_ABORTED_SIGNATURE_INVALID
		}
	}
	c.SendTransactionsToOrderer(t, txs)
	c.ValidateExpectedResultsInCommittedBlock(t, expected)
}
