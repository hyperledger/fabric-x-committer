/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package sidecar

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
	"github.com/hyperledger/fabric-x-committer/api/protonotify"
	"github.com/hyperledger/fabric-x-committer/api/types"
	"github.com/hyperledger/fabric-x-committer/utils/signature"
	"github.com/hyperledger/fabric-x-committer/utils/signature/sigtest"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

// RequireNotifications verifies that the expected notification were received.
func RequireNotifications( //nolint:revive // argument-limit.
	t *testing.T,
	notifyStream protonotify.Notifier_OpenNotificationStreamClient,
	expectedBlockNumber uint64,
	txIDs []string,
	status []protoblocktx.Status,
) {
	t.Helper()
	require.Len(t, status, len(txIDs))
	expected := make([]*protonotify.TxStatusEvent, 0, len(txIDs))
	for i, s := range status {
		if !IsStatusStoredInDB(s) {
			continue
		}
		expected = append(expected, &protonotify.TxStatusEvent{
			TxId:             txIDs[i],
			StatusWithHeight: types.CreateStatusWithHeight(s, expectedBlockNumber, i),
		})
	}

	var actual []*protonotify.TxStatusEvent
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		res, err := notifyStream.Recv()
		require.NoError(t, err)
		require.NotNil(t, res)
		require.Nil(t, res.TimeoutTxIds)
		actual = append(actual, res.TxStatusEvents...)
		test.RequireProtoElementsMatch(ct, expected, actual)
	}, 15*time.Second, 50*time.Millisecond)
}

// MalformedTxTestCases are valid and invalid TXs due to malformed.
var MalformedTxTestCases = []struct {
	Tx             *protoblocktx.Tx
	ExpectedStatus protoblocktx.Status
}{
	{
		Tx: &protoblocktx.Tx{
			Id: "valid TX",
			Namespaces: []*protoblocktx.TxNamespace{
				{
					NsId:      "1",
					NsVersion: 0,
					ReadWrites: []*protoblocktx.ReadWrite{
						{Key: []byte("k1")},
					},
				},
			},
			Signatures: make([][]byte, 1),
		},
		ExpectedStatus: protoblocktx.Status_NOT_VALIDATED,
	},
	{
		Tx:             &protoblocktx.Tx{},
		ExpectedStatus: protoblocktx.Status_MALFORMED_MISSING_TX_ID,
	},
	{
		Tx: &protoblocktx.Tx{
			Id: "empty namespaces",
		},
		ExpectedStatus: protoblocktx.Status_MALFORMED_EMPTY_NAMESPACES,
	},
	{
		Tx: &protoblocktx.Tx{
			Id: "missing signature",
			Namespaces: []*protoblocktx.TxNamespace{
				{
					NsId:       "1",
					NsVersion:  0,
					ReadWrites: []*protoblocktx.ReadWrite{{Key: []byte("1")}},
				},
			},
		},
		ExpectedStatus: protoblocktx.Status_MALFORMED_MISSING_SIGNATURE,
	},
	{
		Tx: &protoblocktx.Tx{
			Id: "not enough signatures",
			Namespaces: []*protoblocktx.TxNamespace{
				{
					NsId:       "1",
					NsVersion:  0,
					ReadWrites: []*protoblocktx.ReadWrite{{Key: []byte("1")}},
				},
			},
			Signatures: make([][]byte, 0),
		},
		ExpectedStatus: protoblocktx.Status_MALFORMED_MISSING_SIGNATURE,
	},
	{
		Tx: &protoblocktx.Tx{
			Id: "too much signatures",
			Namespaces: []*protoblocktx.TxNamespace{
				{
					NsId:       "1",
					NsVersion:  0,
					ReadWrites: []*protoblocktx.ReadWrite{{Key: []byte("1")}},
				},
			},
			Signatures: make([][]byte, 2),
		},
		ExpectedStatus: protoblocktx.Status_MALFORMED_MISSING_SIGNATURE,
	},
	{
		Tx: &protoblocktx.Tx{
			Id: "namespace id is invalid in tx",
			Namespaces: []*protoblocktx.TxNamespace{
				{NsId: "//"},
			},
			Signatures: make([][]byte, 1),
		},
		ExpectedStatus: protoblocktx.Status_MALFORMED_NAMESPACE_ID_INVALID,
	},
	{
		Tx: &protoblocktx.Tx{
			Id: "no writes",
			Namespaces: []*protoblocktx.TxNamespace{
				{
					NsId:      "1",
					NsVersion: 0,
					ReadsOnly: []*protoblocktx.Read{
						{Key: []byte("k1")},
					},
				},
			},
			Signatures: make([][]byte, 1),
		},
		ExpectedStatus: protoblocktx.Status_MALFORMED_NO_WRITES,
	},
	{
		Tx: &protoblocktx.Tx{
			Id: "namespace id is invalid in metaNs tx",
			Namespaces: []*protoblocktx.TxNamespace{
				{
					NsId:      "1",
					NsVersion: 0,
					ReadWrites: []*protoblocktx.ReadWrite{
						{Key: []byte("key")},
					},
				},
				{
					NsId:      types.MetaNamespaceID,
					NsVersion: 0,
					ReadWrites: []*protoblocktx.ReadWrite{
						{Key: []byte("/\\")},
					},
				},
			},
			Signatures: make([][]byte, 2),
		},
		ExpectedStatus: protoblocktx.Status_MALFORMED_NAMESPACE_ID_INVALID,
	},
	{
		Tx: &protoblocktx.Tx{
			Id: "namespace policy is invalid in metaNs tx",
			Namespaces: []*protoblocktx.TxNamespace{
				{
					NsId:      "1",
					NsVersion: 0,
					ReadWrites: []*protoblocktx.ReadWrite{
						{Key: []byte("key")},
					},
				},
				{
					NsId:      types.MetaNamespaceID,
					NsVersion: 0,
					ReadWrites: []*protoblocktx.ReadWrite{
						{
							Key:   []byte("2"),
							Value: []byte("value"),
						},
					},
				},
			},
			Signatures: make([][]byte, 2),
		},
		ExpectedStatus: protoblocktx.Status_MALFORMED_NAMESPACE_POLICY_INVALID,
	},
	{
		Tx: &protoblocktx.Tx{
			Id: "duplicate namespace",
			Namespaces: []*protoblocktx.TxNamespace{
				{
					NsId:      "1",
					NsVersion: 0,
					ReadWrites: []*protoblocktx.ReadWrite{
						{Key: []byte("key")},
					},
				},
				{
					NsId:      "1",
					NsVersion: 0,
				},
			},
			Signatures: make([][]byte, 2),
		},
		ExpectedStatus: protoblocktx.Status_MALFORMED_DUPLICATE_NAMESPACE,
	},
	{
		Tx: &protoblocktx.Tx{
			Id: "valid namespace TX",
			Namespaces: []*protoblocktx.TxNamespace{
				{
					NsId:      types.MetaNamespaceID,
					NsVersion: 0,
					ReadWrites: []*protoblocktx.ReadWrite{
						{
							Key:   []byte("2"),
							Value: defaultNsValidPolicy(),
						},
					},
				},
			},
			Signatures: make([][]byte, 1),
		},
		ExpectedStatus: protoblocktx.Status_NOT_VALIDATED,
	},
	{
		Tx: &protoblocktx.Tx{
			Id: "valid namespace TX with regular TX",
			Namespaces: []*protoblocktx.TxNamespace{
				{
					NsId:      "1",
					NsVersion: 0,
					ReadWrites: []*protoblocktx.ReadWrite{
						{Key: []byte("key")},
					},
				},
				{
					NsId:      types.MetaNamespaceID,
					NsVersion: 0,
					ReadWrites: []*protoblocktx.ReadWrite{
						{
							Key:   []byte("2"),
							Value: defaultNsValidPolicy(),
						},
					},
				},
			},
			Signatures: make([][]byte, 2),
		},
		ExpectedStatus: protoblocktx.Status_NOT_VALIDATED,
	},
	{
		Tx: &protoblocktx.Tx{
			Id: "invalid policy",
			Namespaces: []*protoblocktx.TxNamespace{
				{
					NsId:      types.MetaNamespaceID,
					NsVersion: 0,
					ReadWrites: []*protoblocktx.ReadWrite{
						{
							Key:   []byte("2"),
							Value: defaultNsInvalidPolicy(),
						},
					},
				},
			},
			Signatures: make([][]byte, 1),
		},
		ExpectedStatus: protoblocktx.Status_MALFORMED_NAMESPACE_POLICY_INVALID,
	},
	{
		Tx: &protoblocktx.Tx{
			Id: "blind writes not allowed in metaNs tx",
			Namespaces: []*protoblocktx.TxNamespace{
				{
					NsId:      "1",
					NsVersion: 0,
					ReadWrites: []*protoblocktx.ReadWrite{
						{
							Key: []byte("key"),
						},
					},
				},
				{
					NsId:      types.MetaNamespaceID,
					NsVersion: 0,
					BlindWrites: []*protoblocktx.Write{
						{
							Key:   []byte("2"),
							Value: defaultNsInvalidPolicy(),
						},
					},
				},
			},
			Signatures: make([][]byte, 2),
		},
		ExpectedStatus: protoblocktx.Status_MALFORMED_BLIND_WRITES_NOT_ALLOWED,
	},
	{
		Tx: &protoblocktx.Tx{
			Id: "nil key in ReadOnly",
			Namespaces: []*protoblocktx.TxNamespace{
				{
					NsId:       "1",
					NsVersion:  0,
					ReadsOnly:  []*protoblocktx.Read{{Key: nil}},
					ReadWrites: []*protoblocktx.ReadWrite{{Key: []byte("1")}},
				},
			},
			Signatures: make([][]byte, 1),
		},
		ExpectedStatus: protoblocktx.Status_MALFORMED_EMPTY_KEY,
	},
	{
		Tx: &protoblocktx.Tx{
			Id: "nil key in ReadWrites",
			Namespaces: []*protoblocktx.TxNamespace{
				{
					NsId:       "1",
					NsVersion:  0,
					ReadWrites: []*protoblocktx.ReadWrite{{Key: nil}},
				},
			},
			Signatures: make([][]byte, 1),
		},
		ExpectedStatus: protoblocktx.Status_MALFORMED_EMPTY_KEY,
	},
	{
		Tx: &protoblocktx.Tx{
			Id: "nil key in BlindWrites",
			Namespaces: []*protoblocktx.TxNamespace{
				{
					NsId:        "1",
					NsVersion:   0,
					BlindWrites: []*protoblocktx.Write{{Key: nil}},
				},
			},
			Signatures: make([][]byte, 1),
		},
		ExpectedStatus: protoblocktx.Status_MALFORMED_EMPTY_KEY,
	},
	{
		Tx: &protoblocktx.Tx{
			Id: "duplicate key within ReadOnly",
			Namespaces: []*protoblocktx.TxNamespace{
				{
					NsId:       "1",
					NsVersion:  0,
					ReadsOnly:  []*protoblocktx.Read{{Key: []byte("key1")}, {Key: []byte("key1")}},
					ReadWrites: []*protoblocktx.ReadWrite{{Key: []byte("1")}},
				},
			},
			Signatures: make([][]byte, 1),
		},
		ExpectedStatus: protoblocktx.Status_MALFORMED_DUPLICATE_KEY_IN_READ_WRITE_SET,
	},
	{
		Tx: &protoblocktx.Tx{
			Id: "duplicate key within ReadWrite",
			Namespaces: []*protoblocktx.TxNamespace{
				{
					NsId:       "1",
					NsVersion:  0,
					ReadWrites: []*protoblocktx.ReadWrite{{Key: []byte("key1")}, {Key: []byte("key1")}},
				},
			},
			Signatures: make([][]byte, 1),
		},
		ExpectedStatus: protoblocktx.Status_MALFORMED_DUPLICATE_KEY_IN_READ_WRITE_SET,
	},
	{
		Tx: &protoblocktx.Tx{
			Id: "duplicate key within BlindWrites",
			Namespaces: []*protoblocktx.TxNamespace{
				{
					NsId:        "1",
					NsVersion:   0,
					BlindWrites: []*protoblocktx.Write{{Key: []byte("key1")}, {Key: []byte("key1")}},
				},
			},
			Signatures: make([][]byte, 1),
		},
		ExpectedStatus: protoblocktx.Status_MALFORMED_DUPLICATE_KEY_IN_READ_WRITE_SET,
	},
	{
		Tx: &protoblocktx.Tx{
			Id: "duplicate key across ReadOnly and ReadWrite",
			Namespaces: []*protoblocktx.TxNamespace{
				{
					NsId:       "1",
					NsVersion:  0,
					ReadsOnly:  []*protoblocktx.Read{{Key: []byte("key1")}},
					ReadWrites: []*protoblocktx.ReadWrite{{Key: []byte("key1")}},
				},
			},
			Signatures: make([][]byte, 1),
		},
		ExpectedStatus: protoblocktx.Status_MALFORMED_DUPLICATE_KEY_IN_READ_WRITE_SET,
	},
	{
		Tx: &protoblocktx.Tx{
			Id: "duplicate key across ReadWrite and BlindWrites",
			Namespaces: []*protoblocktx.TxNamespace{
				{
					NsId:        "1",
					NsVersion:   0,
					ReadWrites:  []*protoblocktx.ReadWrite{{Key: []byte("key1")}},
					BlindWrites: []*protoblocktx.Write{{Key: []byte("key1")}},
				},
			},
			Signatures: make([][]byte, 1),
		},
		ExpectedStatus: protoblocktx.Status_MALFORMED_DUPLICATE_KEY_IN_READ_WRITE_SET,
	},
}

func defaultNsInvalidPolicy() []byte {
	nsPolicy, _ := proto.Marshal(&protoblocktx.NamespacePolicy{
		Scheme:    "ECDSA",
		PublicKey: []byte("publicKey"),
	})
	return nsPolicy
}

func defaultNsValidPolicy() []byte {
	factory := sigtest.NewSignatureFactory(signature.Ecdsa)
	_, verificationKey := factory.NewKeys()
	nsPolicy, _ := proto.Marshal(&protoblocktx.NamespacePolicy{
		Scheme:    "ECDSA",
		PublicKey: verificationKey,
	})
	return nsPolicy
}
