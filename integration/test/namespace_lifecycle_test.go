/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"testing"
	"time"

	"github.com/onsi/gomega"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
	"github.com/hyperledger/fabric-x-committer/api/types"
	"github.com/hyperledger/fabric-x-committer/integration/runner"
	"github.com/hyperledger/fabric-x-committer/utils/signature"
)

func TestCreateUpdateNamespace(t *testing.T) {
	t.Parallel()
	gomega.RegisterTestingT(t)
	c := runner.NewRuntime(t, &runner.Config{
		NumVerifiers: 2,
		NumVCService: 2,
		BlockTimeout: 2 * time.Second,
	})
	c.Start(t, runner.FullTxPath)

	cr1 := c.CreateCryptoForNs(t, "1", signature.Ecdsa)
	ns1Policy := cr1.HashSigner.GetVerificationPolicy()
	policyBytes, err := proto.Marshal(ns1Policy)
	require.NoError(t, err)

	cr2 := c.CreateCryptoForNs(t, "2", signature.Ecdsa)
	ns2Policy := cr2.HashSigner.GetVerificationPolicy()
	policyBytesNs2, err := proto.Marshal(ns2Policy)
	require.NoError(t, err)

	tests := []struct {
		name            string
		txs             []*protoblocktx.Tx
		expectedResults *runner.ExpectedStatusInBlock
	}{
		{
			name: "create namespace ns1",
			txs: []*protoblocktx.Tx{
				{
					Id: "create ns 1",
					Namespaces: []*protoblocktx.TxNamespace{
						{
							NsId:      types.MetaNamespaceID,
							NsVersion: 0,
							ReadWrites: []*protoblocktx.ReadWrite{
								{
									Key:   []byte("1"),
									Value: policyBytes,
								},
							},
						},
					},
				},
			},
			expectedResults: &runner.ExpectedStatusInBlock{
				TxIDs:    []string{"create ns 1"},
				Statuses: []protoblocktx.Status{protoblocktx.Status_COMMITTED},
			},
		},
		{
			name: "write to namespace ns1",
			txs: []*protoblocktx.Tx{
				{
					Id: "write to ns 1",
					Namespaces: []*protoblocktx.TxNamespace{
						{
							NsId:      "1",
							NsVersion: 0,
							BlindWrites: []*protoblocktx.Write{
								{
									Key:   []byte("key1"),
									Value: []byte("value1"),
								},
							},
						},
					},
				},
			},
			expectedResults: &runner.ExpectedStatusInBlock{
				TxIDs:    []string{"write to ns 1"},
				Statuses: []protoblocktx.Status{protoblocktx.Status_COMMITTED},
			},
		},
		{
			name: "update namespace ns1",
			txs: []*protoblocktx.Tx{
				{
					Id: "write to ns 1 before updating ns1",
					Namespaces: []*protoblocktx.TxNamespace{
						{
							NsId:      "1",
							NsVersion: 0,
							BlindWrites: []*protoblocktx.Write{
								{
									Key:   []byte("key2"),
									Value: []byte("value2"),
								},
							},
						},
					},
				},
				{
					Id: "update ns 1 with incorrect policy",
					Namespaces: []*protoblocktx.TxNamespace{
						{
							NsId:      types.MetaNamespaceID,
							NsVersion: 0,
							ReadWrites: []*protoblocktx.ReadWrite{
								{
									Key:     []byte("1"),
									Version: types.Version(0),
									Value:   policyBytesNs2,
								},
							},
						},
					},
				},
				{
					Id: "write to stale ns 1 after incorrect policy",
					Namespaces: []*protoblocktx.TxNamespace{
						{
							NsId:      "1",
							NsVersion: 1,
							BlindWrites: []*protoblocktx.Write{
								{
									Key:   []byte("key3"),
									Value: []byte("value3"),
								},
							},
						},
					},
				},
				{
					Id: "update ns 1 with correct policy",
					Namespaces: []*protoblocktx.TxNamespace{
						{
							NsId:      types.MetaNamespaceID,
							NsVersion: 0,
							ReadWrites: []*protoblocktx.ReadWrite{
								{
									Key:     []byte("1"),
									Version: types.Version(1),
									Value:   policyBytes,
								},
							},
						},
					},
				},
				{
					Id: "write to stale ns 1 after correct policy",
					Namespaces: []*protoblocktx.TxNamespace{
						{
							NsId:      "1",
							NsVersion: 1,
							BlindWrites: []*protoblocktx.Write{
								{
									Key:   []byte("key3"),
									Value: []byte("value3"),
								},
							},
						},
					},
				},
			},
			expectedResults: &runner.ExpectedStatusInBlock{
				TxIDs: []string{
					"write to ns 1 before updating ns1",
					"update ns 1 with incorrect policy",
					"write to stale ns 1 after incorrect policy",
					"update ns 1 with correct policy",
					"write to stale ns 1 after correct policy",
				},
				Statuses: []protoblocktx.Status{
					protoblocktx.Status_COMMITTED,
					protoblocktx.Status_COMMITTED,
					protoblocktx.Status_ABORTED_SIGNATURE_INVALID,
					protoblocktx.Status_COMMITTED,
					protoblocktx.Status_ABORTED_MVCC_CONFLICT,
				},
			},
		},
		{
			name: "write again to namespace ns1",
			txs: []*protoblocktx.Tx{
				{
					Id: "write to ns1 again",
					Namespaces: []*protoblocktx.TxNamespace{
						{
							NsId:      "1",
							NsVersion: 2,
							BlindWrites: []*protoblocktx.Write{
								{
									Key:   []byte("key4"),
									Value: []byte("value4"),
								},
							},
						},
					},
				},
			},
			expectedResults: &runner.ExpectedStatusInBlock{
				TxIDs:    []string{"write to ns1 again"},
				Statuses: []protoblocktx.Status{protoblocktx.Status_COMMITTED},
			},
		},
	}

	for _, tt := range tests { //nolint:paralleltest // order is important.
		t.Run(tt.name, func(t *testing.T) {
			for _, tx := range tt.txs {
				c.AddSignatures(t, tx)
			}
			c.SendTransactionsToOrderer(t, tt.txs)

			c.ValidateExpectedResultsInCommittedBlock(t, tt.expectedResults)
		})
	}
}
