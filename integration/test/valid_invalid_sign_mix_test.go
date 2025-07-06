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
)

func TestMixOfValidAndInvalidSign(t *testing.T) { //nolint:gocognit
	t.Parallel()
	gomega.RegisterTestingT(t)
	c := runner.NewRuntime(t, &runner.Config{
		NumVerifiers: 2,
		NumVCService: 2,
		BlockSize:    5,
		BlockTimeout: 2 * time.Second,
	})
	c.Start(t, runner.FullTxPath)
	c.CreateNamespacesAndCommit(t, "1")

	tests := []struct {
		name            string
		txs             []*protoblocktx.Tx
		validSign       []bool
		expectedResults *runner.ExpectedStatusInBlock
	}{
		{
			name: "txs with valid and invalid signs",
			txs: []*protoblocktx.Tx{
				{
					Id: "valid sign 1",
					Namespaces: []*protoblocktx.TxNamespace{
						{
							BlindWrites: []*protoblocktx.Write{
								{
									Key: []byte("k2"),
								},
							},
						},
					},
				},
				{
					Id: "invalid sign 1",
					Namespaces: []*protoblocktx.TxNamespace{
						{
							BlindWrites: []*protoblocktx.Write{
								{
									Key: []byte("k3"),
								},
							},
						},
					},
				},
				{
					Id: "valid sign 2",
					Namespaces: []*protoblocktx.TxNamespace{
						{
							BlindWrites: []*protoblocktx.Write{
								{
									Key: []byte("k4"),
								},
							},
						},
					},
				},
				{
					Id: "invalid sign 2",
					Namespaces: []*protoblocktx.TxNamespace{
						{
							BlindWrites: []*protoblocktx.Write{
								{
									Key: []byte("k5"),
								},
							},
						},
					},
				},
			},
			validSign: []bool{true, false, true, false},
			expectedResults: &runner.ExpectedStatusInBlock{
				TxIDs: []string{"valid sign 1", "invalid sign 1", "valid sign 2", "invalid sign 2"},
				Statuses: []protoblocktx.Status{
					protoblocktx.Status_COMMITTED,
					protoblocktx.Status_ABORTED_SIGNATURE_INVALID,
					protoblocktx.Status_COMMITTED,
					protoblocktx.Status_ABORTED_SIGNATURE_INVALID,
				},
			},
		},
	}

	for _, tt := range tests { //nolint:paralleltest // order is important.
		t.Run(tt.name, func(t *testing.T) {
			for i, tx := range tt.txs {
				for _, ns := range tx.Namespaces {
					ns.NsId = "1"
					ns.NsVersion = 0
				}
				if tt.validSign[i] {
					c.AddSignatures(t, tx)
					continue
				}
				for range len(tx.Namespaces) {
					tx.Signatures = append(tx.Signatures, []byte("dummy"))
				}
			}

			c.SendTransactionsToOrderer(t, tt.txs)
			c.ValidateExpectedResultsInCommittedBlock(t, tt.expectedResults)
		})
	}
}
