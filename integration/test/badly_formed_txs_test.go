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
	"github.com/hyperledger/fabric-x-committer/service/verifier"
	"github.com/hyperledger/fabric-x-committer/utils/signature"
)

func TestBadlyFormedTxs(t *testing.T) {
	t.Parallel()
	gomega.RegisterTestingT(t)
	c := runner.NewRuntime(t, &runner.Config{
		NumVerifiers: 2,
		NumVCService: 2,
		BlockSize:    5,
		BlockTimeout: 1 * time.Second,
	})
	c.Start(t, runner.FullTxPath)

	c.CreateCryptoForNs(t, "1", signature.Ecdsa)

	//nolint:paralleltest
	for _, tt := range verifier.BadTxFormatTestCases {
		t.Run(tt.Tx.Id, func(t *testing.T) {
			c.SendTransactionsToOrderer(t, []*protoblocktx.Tx{tt.Tx})
			c.ValidateExpectedResultsInCommittedBlock(t, &runner.ExpectedStatusInBlock{
				TxIDs: []string{tt.Tx.Id}, Statuses: []protoblocktx.Status{tt.ExpectedStatus},
			})
		})
	}
}
