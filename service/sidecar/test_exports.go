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
	"github.com/hyperledger/fabric-x-committer/api/protoloadgen"
	"github.com/hyperledger/fabric-x-committer/api/protonotify"
	"github.com/hyperledger/fabric-x-committer/api/types"
	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
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
		//nolint:gosec // int -> uint32.
		expected = append(expected, &protonotify.TxStatusEvent{
			TxId:             txIDs[i],
			StatusWithHeight: types.NewStatusWithHeight(s, expectedBlockNumber, uint32(i)),
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
func MalformedTxTestCases(txb *workload.TxBuilder) (
	txs []*protoloadgen.TX, expectedStatuses []protoblocktx.Status,
) {
	add := func(expected protoblocktx.Status, tx *protoloadgen.TX) {
		txs = append(txs, tx)
		expectedStatuses = append(expectedStatuses, expected)
	}

	validTxNamespaces := []*protoblocktx.TxNamespace{{
		NsId:        "1",
		NsVersion:   0,
		BlindWrites: []*protoblocktx.Write{{Key: []byte("k1")}},
	}}
	validTx := txb.MakeTx(&protoblocktx.Tx{Namespaces: validTxNamespaces})

	add(protoblocktx.Status_COMMITTED, validTx)
	add(protoblocktx.Status_REJECTED_DUPLICATE_TX_ID, txb.MakeTxWithID(validTx.Id, &protoblocktx.Tx{
		Namespaces: validTxNamespaces,
	}))
	add(protoblocktx.Status_MALFORMED_MISSING_TX_ID, txb.MakeTxWithID("", &protoblocktx.Tx{
		Namespaces: validTxNamespaces,
	}))
	add(protoblocktx.Status_MALFORMED_EMPTY_NAMESPACES, txb.MakeTx(&protoblocktx.Tx{}))
	add(protoblocktx.Status_MALFORMED_EMPTY_NAMESPACES, txb.MakeTx(&protoblocktx.Tx{
		Namespaces: make([]*protoblocktx.TxNamespace, 0),
	}))
	add(protoblocktx.Status_MALFORMED_MISSING_SIGNATURE, txb.MakeTx(&protoblocktx.Tx{
		Namespaces: validTxNamespaces,
		Signatures: make([][]byte, 0), // Not enough signatures.
	}))
	add(protoblocktx.Status_MALFORMED_MISSING_SIGNATURE, txb.MakeTx(&protoblocktx.Tx{
		Namespaces: validTxNamespaces,
		Signatures: make([][]byte, 2), // Too many signatures.
	}))
	add(protoblocktx.Status_MALFORMED_NO_WRITES, txb.MakeTx(&protoblocktx.Tx{
		Namespaces: []*protoblocktx.TxNamespace{{
			NsId:      "1",
			NsVersion: 0,
			ReadsOnly: []*protoblocktx.Read{{Key: []byte("k1")}},
		}},
	}))
	add(protoblocktx.Status_MALFORMED_NAMESPACE_ID_INVALID, txb.MakeTx(&protoblocktx.Tx{
		Namespaces: []*protoblocktx.TxNamespace{
			// namespace id is invalid.
			{NsId: "//", BlindWrites: validTxNamespaces[0].BlindWrites},
		},
	}))
	add(protoblocktx.Status_MALFORMED_NAMESPACE_ID_INVALID, txb.MakeTx(&protoblocktx.Tx{
		Namespaces: []*protoblocktx.TxNamespace{
			validTxNamespaces[0],
			{
				NsId:      types.MetaNamespaceID,
				NsVersion: 0,
				// namespace id is invalid in metaNs tx.
				ReadWrites: []*protoblocktx.ReadWrite{{Key: []byte("/\\")}},
			},
		},
	}))
	add(protoblocktx.Status_MALFORMED_NAMESPACE_POLICY_INVALID, txb.MakeTx(&protoblocktx.Tx{
		Namespaces: []*protoblocktx.TxNamespace{
			validTxNamespaces[0],
			{
				NsId:      types.MetaNamespaceID,
				NsVersion: 0,
				ReadWrites: []*protoblocktx.ReadWrite{{
					Key:   []byte("2"),
					Value: []byte("not a real policy"),
				}},
			},
		},
	}))
	add(protoblocktx.Status_MALFORMED_DUPLICATE_NAMESPACE, txb.MakeTx(&protoblocktx.Tx{
		Namespaces: []*protoblocktx.TxNamespace{validTxNamespaces[0], validTxNamespaces[0]},
	}))
	add(protoblocktx.Status_COMMITTED, txb.MakeTx(&protoblocktx.Tx{
		Namespaces: []*protoblocktx.TxNamespace{{
			// valid namespace TX.
			NsId:      types.MetaNamespaceID,
			NsVersion: 0,
			ReadWrites: []*protoblocktx.ReadWrite{{
				Key:   []byte("2"),
				Value: defaultNsValidPolicy(),
			}},
		}},
	}))
	add(protoblocktx.Status_COMMITTED, txb.MakeTx(&protoblocktx.Tx{
		Namespaces: []*protoblocktx.TxNamespace{
			// valid namespace TX with regular TX.
			validTxNamespaces[0],
			{
				NsId:      types.MetaNamespaceID,
				NsVersion: 0,
				ReadWrites: []*protoblocktx.ReadWrite{{
					Key:     []byte("2"),
					Version: types.Version(0),
					Value:   defaultNsValidPolicy(),
				}},
			},
		},
	}))
	add(protoblocktx.Status_MALFORMED_NAMESPACE_POLICY_INVALID, txb.MakeTx(&protoblocktx.Tx{
		Namespaces: []*protoblocktx.TxNamespace{{
			NsId:      types.MetaNamespaceID,
			NsVersion: 0,
			ReadWrites: []*protoblocktx.ReadWrite{{
				Key:     []byte("2"),
				Version: types.Version(0),
				Value:   defaultNsInvalidPolicy(), // invalid policy.
			}},
		}},
	}))
	add(protoblocktx.Status_MALFORMED_BLIND_WRITES_NOT_ALLOWED, txb.MakeTx(&protoblocktx.Tx{
		Namespaces: []*protoblocktx.TxNamespace{
			validTxNamespaces[0],
			{
				NsId:      types.MetaNamespaceID,
				NsVersion: 0,
				// blind writes not allowed in metaNs tx.
				BlindWrites: []*protoblocktx.Write{{
					Key:   []byte("2"),
					Value: defaultNsInvalidPolicy(),
				}},
			},
		},
	}))
	add(protoblocktx.Status_MALFORMED_EMPTY_KEY, txb.MakeTx(&protoblocktx.Tx{
		Namespaces: []*protoblocktx.TxNamespace{{
			NsId:       "1",
			NsVersion:  0,
			ReadsOnly:  []*protoblocktx.Read{{Key: nil}},
			ReadWrites: []*protoblocktx.ReadWrite{{Key: []byte("1")}},
		}},
	}))
	add(protoblocktx.Status_MALFORMED_EMPTY_KEY, txb.MakeTx(&protoblocktx.Tx{
		Namespaces: []*protoblocktx.TxNamespace{{
			NsId:       "1",
			NsVersion:  0,
			ReadWrites: []*protoblocktx.ReadWrite{{Key: nil}},
		}},
	}))
	add(protoblocktx.Status_MALFORMED_EMPTY_KEY, txb.MakeTx(&protoblocktx.Tx{
		Namespaces: []*protoblocktx.TxNamespace{{
			NsId:        "1",
			NsVersion:   0,
			BlindWrites: []*protoblocktx.Write{{Key: nil}},
		}},
	}))
	add(protoblocktx.Status_MALFORMED_DUPLICATE_KEY_IN_READ_WRITE_SET, txb.MakeTx(&protoblocktx.Tx{
		Namespaces: []*protoblocktx.TxNamespace{{
			NsId:       "1",
			NsVersion:  0,
			ReadsOnly:  []*protoblocktx.Read{{Key: []byte("key1")}, {Key: []byte("key1")}},
			ReadWrites: []*protoblocktx.ReadWrite{{Key: []byte("1")}},
		}},
	}))
	add(protoblocktx.Status_MALFORMED_DUPLICATE_KEY_IN_READ_WRITE_SET, txb.MakeTx(&protoblocktx.Tx{
		Namespaces: []*protoblocktx.TxNamespace{{
			NsId:       "1",
			NsVersion:  0,
			ReadWrites: []*protoblocktx.ReadWrite{{Key: []byte("key1")}, {Key: []byte("key1")}},
		}},
	}))
	add(protoblocktx.Status_MALFORMED_DUPLICATE_KEY_IN_READ_WRITE_SET, txb.MakeTx(&protoblocktx.Tx{
		Namespaces: []*protoblocktx.TxNamespace{{
			NsId:        "1",
			NsVersion:   0,
			BlindWrites: []*protoblocktx.Write{{Key: []byte("key1")}, {Key: []byte("key1")}},
		}},
	}))
	add(protoblocktx.Status_MALFORMED_DUPLICATE_KEY_IN_READ_WRITE_SET, txb.MakeTx(&protoblocktx.Tx{
		Namespaces: []*protoblocktx.TxNamespace{{
			NsId:       "1",
			NsVersion:  0,
			ReadsOnly:  []*protoblocktx.Read{{Key: []byte("key1")}},
			ReadWrites: []*protoblocktx.ReadWrite{{Key: []byte("key1")}},
		}},
	}))
	add(protoblocktx.Status_MALFORMED_DUPLICATE_KEY_IN_READ_WRITE_SET, txb.MakeTx(&protoblocktx.Tx{
		Namespaces: []*protoblocktx.TxNamespace{{
			NsId:        "1",
			NsVersion:   0,
			ReadWrites:  []*protoblocktx.ReadWrite{{Key: []byte("key1")}},
			BlindWrites: []*protoblocktx.Write{{Key: []byte("key1")}},
		}},
	}))
	return txs, expectedStatuses
}

func defaultNsInvalidPolicy() []byte {
	nsPolicy, _ := proto.Marshal(&protoblocktx.NamespacePolicy{
		Scheme:    signature.Ecdsa,
		PublicKey: []byte("publicKey"),
	})
	return nsPolicy
}

func defaultNsValidPolicy() []byte {
	factory := sigtest.NewSignatureFactory(signature.Ecdsa)
	_, verificationKey := factory.NewKeys()
	nsPolicy, _ := proto.Marshal(&protoblocktx.NamespacePolicy{
		Scheme:    signature.Ecdsa,
		PublicKey: verificationKey,
	})
	return nsPolicy
}
