/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"testing"
	"time"

	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
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

	verPolicies := c.TxBuilder.TxEndorser.VerificationPolicies()
	policyBytesNs1, err := proto.Marshal(verPolicies["1"])
	require.NoError(t, err)
	policyBytesNs2, err := proto.Marshal(verPolicies["2"])
	require.NoError(t, err)

	tests := []struct {
		name     string
		txs      [][]*applicationpb.TxNamespace
		expected []committerpb.Status
	}{
		{
			name: "create namespace ns1",
			txs: [][]*applicationpb.TxNamespace{{{ // create ns 1.
				NsId:      committerpb.MetaNamespaceID,
				NsVersion: 0,
				ReadWrites: []*applicationpb.ReadWrite{{
					Key:   []byte("1"),
					Value: policyBytesNs1,
				}},
			}}},
			expected: []committerpb.Status{committerpb.Status_COMMITTED},
		},
		{
			name: "write to namespace ns1",
			txs: [][]*applicationpb.TxNamespace{{{ // write to ns 1.
				NsId:      "1",
				NsVersion: 0,
				BlindWrites: []*applicationpb.Write{{
					Key:   []byte("key1"),
					Value: []byte("value1"),
				}},
			}}},
			expected: []committerpb.Status{committerpb.Status_COMMITTED},
		},
		{
			name: "update namespace ns1",
			txs: [][]*applicationpb.TxNamespace{
				{{ // write to ns 1 before updating ns1.
					NsId:      "1",
					NsVersion: 0,
					BlindWrites: []*applicationpb.Write{{
						Key:   []byte("key2"),
						Value: []byte("value2"),
					}},
				}},
				{{ // update ns 1 with incorrect policy.
					NsId:      committerpb.MetaNamespaceID,
					NsVersion: 0,
					ReadWrites: []*applicationpb.ReadWrite{{
						Key:     []byte("1"),
						Version: applicationpb.NewVersion(0),
						Value:   policyBytesNs2,
					}},
				}},
				{{ // write to stale ns 1 after incorrect policy.
					NsId:      "1",
					NsVersion: 1,
					BlindWrites: []*applicationpb.Write{{
						Key:   []byte("key3"),
						Value: []byte("value3"),
					}},
				}},
				{{ // update ns 1 with correct policy.
					NsId:      committerpb.MetaNamespaceID,
					NsVersion: 0,
					ReadWrites: []*applicationpb.ReadWrite{{
						Key:     []byte("1"),
						Version: applicationpb.NewVersion(1),
						Value:   policyBytesNs1,
					}},
				}},
				{{ // write to stale ns 1 after correct policy.
					NsId:      "1",
					NsVersion: 1,
					BlindWrites: []*applicationpb.Write{{
						Key:   []byte("key3"),
						Value: []byte("value3"),
					}},
				}},
			},
			expected: []committerpb.Status{
				committerpb.Status_COMMITTED,
				committerpb.Status_COMMITTED,
				committerpb.Status_ABORTED_SIGNATURE_INVALID,
				committerpb.Status_COMMITTED,
				committerpb.Status_ABORTED_MVCC_CONFLICT,
			},
		},
		{
			name: "write again to namespace ns1",
			txs: [][]*applicationpb.TxNamespace{{{ // write to ns1 again.
				NsId:      "1",
				NsVersion: 2,
				BlindWrites: []*applicationpb.Write{{
					Key:   []byte("key4"),
					Value: []byte("value4"),
				}},
			}}},
			expected: []committerpb.Status{committerpb.Status_COMMITTED},
		},
	}

	for _, tt := range tests { //nolint:paralleltest // order is important.
		t.Run(tt.name, func(t *testing.T) {
			c.MakeAndSendTransactionsToOrderer(t, tt.txs, tt.expected)
		})
	}

	t.Run("empty endorsements", func(t *testing.T) {
		tx := &applicationpb.Tx{
			Namespaces: []*applicationpb.TxNamespace{{
				NsId:      "1",
				NsVersion: 2,
				BlindWrites: []*applicationpb.Write{{
					Key:   []byte("key5"),
					Value: []byte("value5"),
				}},
			}},
			Endorsements: []*applicationpb.Endorsements{{
				EndorsementsWithIdentity: []*applicationpb.EndorsementWithIdentity{},
			}},
		}
		lgTx := c.TxBuilder.MakeTx(tx)
		c.SendTransactionsToOrderer(t,
			[]*servicepb.LoadGenTx{lgTx},
			[]committerpb.Status{committerpb.Status_MALFORMED_MISSING_SIGNATURE},
		)
	})
}
