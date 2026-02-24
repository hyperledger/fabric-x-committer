/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package sidecar

import (
	"testing"
	"time"

	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/hyperledger/fabric-x-common/protoutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/service/verifier/policy"
	"github.com/hyperledger/fabric-x-committer/utils/signature"
	"github.com/hyperledger/fabric-x-committer/utils/test"
	"github.com/hyperledger/fabric-x-committer/utils/testsig"
)

// RequireNotifications verifies that the expected notification were received.
func RequireNotifications( //nolint:revive // argument-limit.
	t *testing.T,
	notifyStream committerpb.Notifier_OpenNotificationStreamClient,
	expectedBlockNumber uint64,
	txIDs []string,
	status []committerpb.Status,
) {
	t.Helper()
	require.Len(t, status, len(txIDs))
	expected := make([]*committerpb.TxStatus, 0, len(txIDs))
	for i, s := range status {
		if !IsStatusStoredInDB(s) {
			continue
		}
		//nolint:gosec // int -> uint32.
		expected = append(expected, &committerpb.TxStatus{
			Ref:    committerpb.NewTxRef(txIDs[i], expectedBlockNumber, uint32(i)),
			Status: s,
		})
	}

	var actual []*committerpb.TxStatus
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
	txs []*servicepb.LoadGenTx, expectedStatuses []committerpb.Status,
) {
	add := func(expected committerpb.Status, tx *servicepb.LoadGenTx) {
		txs = append(txs, tx)
		expectedStatuses = append(expectedStatuses, expected)
	}

	// placeholderEndorsements returns dummy endorsements when no TxEndorser is available
	// (e.g., sidecar unit tests that only check form validation). When a TxEndorser is
	// present (e.g., integration tests), it returns nil so MakeTx uses the TxEndorser
	// to produce real signatures that pass verification.
	placeholderEndorsements := func(n int) []*applicationpb.Endorsements {
		if txb.TxEndorser == nil {
			return dummyEndorsements(n)
		}
		return nil
	}

	validTxNamespaces := []*applicationpb.TxNamespace{{
		NsId:        "1",
		NsVersion:   0,
		BlindWrites: []*applicationpb.Write{{Key: []byte("k1")}},
	}}
	validTx := txb.MakeTx(&applicationpb.Tx{Namespaces: validTxNamespaces, Endorsements: placeholderEndorsements(1)})

	add(committerpb.Status_COMMITTED, validTx)
	add(committerpb.Status_REJECTED_DUPLICATE_TX_ID, txb.MakeTxWithID(validTx.Id, &applicationpb.Tx{
		Namespaces: validTxNamespaces, Endorsements: dummyEndorsements(1),
	}))
	add(committerpb.Status_MALFORMED_MISSING_TX_ID, txb.MakeTxWithID("", &applicationpb.Tx{
		Namespaces: validTxNamespaces, Endorsements: dummyEndorsements(1),
	}))
	add(committerpb.Status_MALFORMED_EMPTY_NAMESPACES, txb.MakeTx(&applicationpb.Tx{}))
	add(committerpb.Status_MALFORMED_EMPTY_NAMESPACES, txb.MakeTx(&applicationpb.Tx{
		Namespaces: make([]*applicationpb.TxNamespace, 0),
	}))
	add(committerpb.Status_MALFORMED_MISSING_SIGNATURE, txb.MakeTx(&applicationpb.Tx{
		Namespaces:   append(validTxNamespaces, nil),
		Endorsements: make([]*applicationpb.Endorsements, 1), // Not enough signatures.
	}))
	add(committerpb.Status_MALFORMED_MISSING_SIGNATURE, txb.MakeTx(&applicationpb.Tx{
		Namespaces:   validTxNamespaces,
		Endorsements: make([]*applicationpb.Endorsements, 2), // Too many signatures.
	}))
	add(committerpb.Status_MALFORMED_MISSING_SIGNATURE, txb.MakeTx(&applicationpb.Tx{
		Namespaces: validTxNamespaces,
		Endorsements: []*applicationpb.Endorsements{{
			EndorsementsWithIdentity: []*applicationpb.EndorsementWithIdentity{}, // Empty inner slice.
		}},
	}))
	add(committerpb.Status_MALFORMED_MISSING_SIGNATURE, txb.MakeTx(&applicationpb.Tx{
		Namespaces: validTxNamespaces,
		Endorsements: []*applicationpb.Endorsements{{
			EndorsementsWithIdentity: []*applicationpb.EndorsementWithIdentity{nil}, // Nil endorsement entry.
		}},
	}))
	add(committerpb.Status_MALFORMED_MISSING_SIGNATURE, txb.MakeTx(&applicationpb.Tx{
		Namespaces: validTxNamespaces,
		Endorsements: []*applicationpb.Endorsements{{
			EndorsementsWithIdentity: []*applicationpb.EndorsementWithIdentity{{
				Endorsement: nil, // Nil signature bytes.
			}},
		}},
	}))
	add(committerpb.Status_MALFORMED_NO_WRITES, txb.MakeTx(&applicationpb.Tx{
		Namespaces: []*applicationpb.TxNamespace{{
			NsId:      "1",
			NsVersion: 0,
			ReadsOnly: []*applicationpb.Read{{Key: []byte("k1")}},
		}},
		Endorsements: dummyEndorsements(1),
	}))
	add(committerpb.Status_MALFORMED_NAMESPACE_ID_INVALID, txb.MakeTx(&applicationpb.Tx{
		Namespaces: []*applicationpb.TxNamespace{
			// namespace id is invalid.
			{NsId: "//", BlindWrites: validTxNamespaces[0].BlindWrites},
		},
		Endorsements: dummyEndorsements(1),
	}))
	add(committerpb.Status_MALFORMED_NAMESPACE_ID_INVALID, txb.MakeTx(&applicationpb.Tx{
		Namespaces: []*applicationpb.TxNamespace{
			validTxNamespaces[0],
			{
				NsId:      committerpb.MetaNamespaceID,
				NsVersion: 0,
				// namespace id is invalid in metaNs tx.
				ReadWrites: []*applicationpb.ReadWrite{{Key: []byte("/\\")}},
			},
		},
		Endorsements: dummyEndorsements(2),
	}))
	add(committerpb.Status_MALFORMED_NAMESPACE_POLICY_INVALID, txb.MakeTx(&applicationpb.Tx{
		Namespaces: []*applicationpb.TxNamespace{
			validTxNamespaces[0],
			{
				NsId:      committerpb.MetaNamespaceID,
				NsVersion: 0,
				ReadWrites: []*applicationpb.ReadWrite{{
					Key:   []byte("2"),
					Value: []byte("not a real policy"),
				}},
			},
		},
		Endorsements: dummyEndorsements(2),
	}))
	add(committerpb.Status_MALFORMED_DUPLICATE_NAMESPACE, txb.MakeTx(&applicationpb.Tx{
		Namespaces:   []*applicationpb.TxNamespace{validTxNamespaces[0], validTxNamespaces[0]},
		Endorsements: dummyEndorsements(2),
	}))
	add(committerpb.Status_COMMITTED, txb.MakeTx(&applicationpb.Tx{
		Namespaces: []*applicationpb.TxNamespace{{
			// valid namespace TX.
			NsId:      committerpb.MetaNamespaceID,
			NsVersion: 0,
			ReadWrites: []*applicationpb.ReadWrite{{
				Key:   []byte("2"),
				Value: defaultNsValidPolicy(),
			}},
		}},
		Endorsements: placeholderEndorsements(1),
	}))
	add(committerpb.Status_COMMITTED, txb.MakeTx(&applicationpb.Tx{
		Namespaces: []*applicationpb.TxNamespace{
			// valid namespace TX with regular TX.
			validTxNamespaces[0],
			{
				NsId:      committerpb.MetaNamespaceID,
				NsVersion: 0,
				ReadWrites: []*applicationpb.ReadWrite{{
					Key:     []byte("2"),
					Version: applicationpb.NewVersion(0),
					Value:   defaultNsValidPolicy(),
				}},
			},
		},
		Endorsements: placeholderEndorsements(2),
	}))
	add(committerpb.Status_MALFORMED_NAMESPACE_POLICY_INVALID, txb.MakeTx(&applicationpb.Tx{
		Namespaces: []*applicationpb.TxNamespace{{
			NsId:      committerpb.MetaNamespaceID,
			NsVersion: 0,
			ReadWrites: []*applicationpb.ReadWrite{{
				Key:     []byte("2"),
				Version: applicationpb.NewVersion(0),
				Value:   defaultNsInvalidPolicy(), // invalid policy.
			}},
		}},
		Endorsements: dummyEndorsements(1),
	}))
	add(committerpb.Status_MALFORMED_BLIND_WRITES_NOT_ALLOWED, txb.MakeTx(&applicationpb.Tx{
		Namespaces: []*applicationpb.TxNamespace{
			validTxNamespaces[0],
			{
				NsId:      committerpb.MetaNamespaceID,
				NsVersion: 0,
				// blind writes not allowed in metaNs tx.
				BlindWrites: []*applicationpb.Write{{
					Key:   []byte("2"),
					Value: defaultNsInvalidPolicy(),
				}},
			},
		},
		Endorsements: dummyEndorsements(2),
	}))
	add(committerpb.Status_MALFORMED_EMPTY_KEY, txb.MakeTx(&applicationpb.Tx{
		Namespaces: []*applicationpb.TxNamespace{{
			NsId:       "1",
			NsVersion:  0,
			ReadsOnly:  []*applicationpb.Read{{Key: nil}},
			ReadWrites: []*applicationpb.ReadWrite{{Key: []byte("1")}},
		}},
		Endorsements: dummyEndorsements(1),
	}))
	add(committerpb.Status_MALFORMED_EMPTY_KEY, txb.MakeTx(&applicationpb.Tx{
		Namespaces: []*applicationpb.TxNamespace{{
			NsId:       "1",
			NsVersion:  0,
			ReadWrites: []*applicationpb.ReadWrite{{Key: nil}},
		}},
		Endorsements: dummyEndorsements(1),
	}))
	add(committerpb.Status_MALFORMED_EMPTY_KEY, txb.MakeTx(&applicationpb.Tx{
		Namespaces: []*applicationpb.TxNamespace{{
			NsId:        "1",
			NsVersion:   0,
			BlindWrites: []*applicationpb.Write{{Key: nil}},
		}},
		Endorsements: dummyEndorsements(1),
	}))
	add(committerpb.Status_MALFORMED_DUPLICATE_KEY_IN_READ_WRITE_SET, txb.MakeTx(&applicationpb.Tx{
		Namespaces: []*applicationpb.TxNamespace{{
			NsId:       "1",
			NsVersion:  0,
			ReadsOnly:  []*applicationpb.Read{{Key: []byte("key1")}, {Key: []byte("key1")}},
			ReadWrites: []*applicationpb.ReadWrite{{Key: []byte("1")}},
		}},
		Endorsements: dummyEndorsements(1),
	}))
	add(committerpb.Status_MALFORMED_DUPLICATE_KEY_IN_READ_WRITE_SET, txb.MakeTx(&applicationpb.Tx{
		Namespaces: []*applicationpb.TxNamespace{{
			NsId:       "1",
			NsVersion:  0,
			ReadWrites: []*applicationpb.ReadWrite{{Key: []byte("key1")}, {Key: []byte("key1")}},
		}},
		Endorsements: dummyEndorsements(1),
	}))
	add(committerpb.Status_MALFORMED_DUPLICATE_KEY_IN_READ_WRITE_SET, txb.MakeTx(&applicationpb.Tx{
		Namespaces: []*applicationpb.TxNamespace{{
			NsId:        "1",
			NsVersion:   0,
			BlindWrites: []*applicationpb.Write{{Key: []byte("key1")}, {Key: []byte("key1")}},
		}},
		Endorsements: dummyEndorsements(1),
	}))
	add(committerpb.Status_MALFORMED_DUPLICATE_KEY_IN_READ_WRITE_SET, txb.MakeTx(&applicationpb.Tx{
		Namespaces: []*applicationpb.TxNamespace{{
			NsId:       "1",
			NsVersion:  0,
			ReadsOnly:  []*applicationpb.Read{{Key: []byte("key1")}},
			ReadWrites: []*applicationpb.ReadWrite{{Key: []byte("key1")}},
		}},
		Endorsements: dummyEndorsements(1),
	}))
	add(committerpb.Status_MALFORMED_DUPLICATE_KEY_IN_READ_WRITE_SET, txb.MakeTx(&applicationpb.Tx{
		Namespaces: []*applicationpb.TxNamespace{{
			NsId:        "1",
			NsVersion:   0,
			ReadWrites:  []*applicationpb.ReadWrite{{Key: []byte("key1")}},
			BlindWrites: []*applicationpb.Write{{Key: []byte("key1")}},
		}},
		Endorsements: dummyEndorsements(1),
	}))
	return txs, expectedStatuses
}

func defaultNsInvalidPolicy() []byte {
	return protoutil.MarshalOrPanic(policy.MakeECDSAThresholdRuleNsPolicy([]byte("public-key")))
}

func defaultNsValidPolicy() []byte {
	_, verificationKey := testsig.NewKeyPair(signature.Ecdsa)
	return protoutil.MarshalOrPanic(policy.MakeECDSAThresholdRuleNsPolicy(verificationKey))
}

func dummyEndorsements(n int) []*applicationpb.Endorsements {
	e := make([]*applicationpb.Endorsements, n)
	for i := range e {
		e[i] = &applicationpb.Endorsements{
			EndorsementsWithIdentity: []*applicationpb.EndorsementWithIdentity{{
				Endorsement: []byte("placeholder"),
			}},
		}
	}
	return e
}
