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

	policyBytesNs1, err := proto.Marshal(c.TxBuilder.TxSigner.HashSigners["1"].GetVerificationPolicy())
	require.NoError(t, err)
	policyBytesNs2, err := proto.Marshal(c.TxBuilder.TxSigner.HashSigners["2"].GetVerificationPolicy())
	require.NoError(t, err)

	tests := []struct {
		name     string
		txs      [][]*protoblocktx.TxNamespace
		expected []protoblocktx.Status
	}{
		{
			name: "create namespace ns1",
			txs: [][]*protoblocktx.TxNamespace{{{ // create ns 1.
				NsId:      types.MetaNamespaceID,
				NsVersion: 0,
				ReadWrites: []*protoblocktx.ReadWrite{{
					Key:   []byte("1"),
					Value: policyBytesNs1,
				}},
			}}},
			expected: []protoblocktx.Status{protoblocktx.Status_COMMITTED},
		},
		{
			name: "write to namespace ns1",
			txs: [][]*protoblocktx.TxNamespace{{{ // write to ns 1.
				NsId:      "1",
				NsVersion: 0,
				BlindWrites: []*protoblocktx.Write{{
					Key:   []byte("key1"),
					Value: []byte("value1"),
				}},
			}}},
			expected: []protoblocktx.Status{protoblocktx.Status_COMMITTED},
		},
		{
			name: "update namespace ns1",
			txs: [][]*protoblocktx.TxNamespace{
				{{ // write to ns 1 before updating ns1.
					NsId:      "1",
					NsVersion: 0,
					BlindWrites: []*protoblocktx.Write{{
						Key:   []byte("key2"),
						Value: []byte("value2"),
					}},
				}},
				{{ // update ns 1 with incorrect policy.
					NsId:      types.MetaNamespaceID,
					NsVersion: 0,
					ReadWrites: []*protoblocktx.ReadWrite{{
						Key:     []byte("1"),
						Version: types.Version(0),
						Value:   policyBytesNs2,
					}},
				}},
				{{ // write to stale ns 1 after incorrect policy.
					NsId:      "1",
					NsVersion: 1,
					BlindWrites: []*protoblocktx.Write{{
						Key:   []byte("key3"),
						Value: []byte("value3"),
					}},
				}},
				{{ // update ns 1 with correct policy.
					NsId:      types.MetaNamespaceID,
					NsVersion: 0,
					ReadWrites: []*protoblocktx.ReadWrite{{
						Key:     []byte("1"),
						Version: types.Version(1),
						Value:   policyBytesNs1,
					}},
				}},
				{{ // write to stale ns 1 after correct policy.
					NsId:      "1",
					NsVersion: 1,
					BlindWrites: []*protoblocktx.Write{{
						Key:   []byte("key3"),
						Value: []byte("value3"),
					}},
				}},
			},
			expected: []protoblocktx.Status{
				protoblocktx.Status_COMMITTED,
				protoblocktx.Status_COMMITTED,
				protoblocktx.Status_ABORTED_SIGNATURE_INVALID,
				protoblocktx.Status_COMMITTED,
				protoblocktx.Status_ABORTED_MVCC_CONFLICT,
			},
		},
		{
			name: "write again to namespace ns1",
			txs: [][]*protoblocktx.TxNamespace{{{ // write to ns1 again.
				NsId:      "1",
				NsVersion: 2,
				BlindWrites: []*protoblocktx.Write{{
					Key:   []byte("key4"),
					Value: []byte("value4"),
				}},
			}}},
			expected: []protoblocktx.Status{protoblocktx.Status_COMMITTED},
		},
	}

	for _, tt := range tests { //nolint:paralleltest // order is important.
		t.Run(tt.name, func(t *testing.T) {
			c.MakeAndSendTransactionsToOrderer(t, tt.txs, tt.expected)
		})
	}
}
