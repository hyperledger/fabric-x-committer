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
		name     string
		txs      [][]*protoblocktx.TxNamespace
		expected []protoblocktx.Status
	}{
		{
			name: "txs with valid and invalid signs",
			txs: [][]*protoblocktx.TxNamespace{
				{{ // valid sign 1.
					BlindWrites: []*protoblocktx.Write{{
						Key: []byte("k2"),
					}},
				}},
				{{ // invalid sign 1.
					BlindWrites: []*protoblocktx.Write{{
						Key: []byte("k3"),
					}},
				}},
				{{ // valid sign 2.
					BlindWrites: []*protoblocktx.Write{{
						Key: []byte("k4"),
					}},
				}},
				{{ // invalid sign 2.
					BlindWrites: []*protoblocktx.Write{{
						Key: []byte("k5"),
					}},
				}},
			},
			expected: []protoblocktx.Status{
				protoblocktx.Status_COMMITTED,
				protoblocktx.Status_ABORTED_SIGNATURE_INVALID,
				protoblocktx.Status_COMMITTED,
				protoblocktx.Status_ABORTED_SIGNATURE_INVALID,
			},
		},
	}

	for _, tt := range tests { //nolint:paralleltest // order is important.
		t.Run(tt.name, func(t *testing.T) {
			for _, tx := range tt.txs {
				for _, ns := range tx {
					ns.NsId = "1"
					ns.NsVersion = 0
				}
			}
			c.MakeAndSendTransactionsToOrderer(t, tt.txs, tt.expected)
		})
	}
}
