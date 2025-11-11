/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package signature_test

import (
	"testing"

	"github.com/hyperledger/fabric-x-common/common/cauthdsl"
	"github.com/hyperledger/fabric-x-common/common/policydsl"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
	"github.com/hyperledger/fabric-x-committer/service/verifier/policy"
	"github.com/hyperledger/fabric-x-committer/utils/signature"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

const fakeTxID = "fake-id"

func TestNsVerifierThresholdRule(t *testing.T) {
	t.Parallel()
	p, nsSigner := policy.MakePolicyAndNsSigner(t, "1")
	nsVerifier, err := signature.NewNsVerifier(p.Policy, nil)
	require.NoError(t, err)

	tx1 := &protoblocktx.Tx{
		Namespaces: []*protoblocktx.TxNamespace{{
			NsId:       "1",
			NsVersion:  1,
			ReadWrites: []*protoblocktx.ReadWrite{{Key: []byte("k1"), Value: []byte("v1")}},
		}},
	}
	sig, err := nsSigner.SignNs(fakeTxID, tx1, 0)
	require.NoError(t, err)
	tx1.Endorsements = test.CreateEndorsementsForThresholdRule(sig)
	require.NoError(t, nsVerifier.VerifyNs(fakeTxID, tx1, 0))
}

func TestNsVerifierSignatureRule(t *testing.T) {
	t.Parallel()
	identities := signature.CreateSerializedIdentities([]string{"org0", "org1", "org2", "org3"},
		[]string{"id0", "id1", "id2", "id3"})

	// org0 and org3 must sign along with either org1 or org2. To realize this condition, the policy can be
	// written in many ways but we choose the following to test the nested structure.
	p := policydsl.Envelope(
		policydsl.And(
			policydsl.Or(
				policydsl.And(policydsl.SignedBy(0), policydsl.SignedBy(1)),
				policydsl.And(policydsl.SignedBy(0), policydsl.SignedBy(2)),
			),
			policydsl.SignedBy(3),
		), identities)
	pBytes, err := proto.Marshal(p)
	require.NoError(t, err)

	nsVerifier, err := signature.NewNsVerifier(
		&protoblocktx.NamespacePolicy{Rule: &protoblocktx.NamespacePolicy_SignatureRule{SignatureRule: pBytes}},
		&cauthdsl.MockIdentityDeserializer{})
	require.NoError(t, err)

	tx1 := &protoblocktx.Tx{
		Namespaces: []*protoblocktx.TxNamespace{{
			NsId:       "1",
			NsVersion:  1,
			ReadWrites: []*protoblocktx.ReadWrite{{Key: []byte("k1"), Value: []byte("v1")}},
		}},
	}
	// org0, org3, and org1 sign.
	tx1.Endorsements = []*protoblocktx.Endorsements{test.CreateEndorsementsForSignatureRule(
		toByteArray("s0", "s3", "s1"),
		toByteArray("org0", "org3", "org1"),
		toByteArray("id0", "id3", "id1"),
	)}
	require.NoError(t, nsVerifier.VerifyNs(fakeTxID, tx1, 0))

	// org0, org3, and org2 sign.
	tx1.Endorsements = []*protoblocktx.Endorsements{test.CreateEndorsementsForSignatureRule(
		toByteArray("s0", "s3", "s2"),
		toByteArray("org0", "org3", "org2"),
		toByteArray("id0", "id3", "id2"),
	)}
	require.NoError(t, nsVerifier.VerifyNs(fakeTxID, tx1, 0))

	tx1.Endorsements = []*protoblocktx.Endorsements{test.CreateEndorsementsForSignatureRule(
		toByteArray("s0", "s3"),
		toByteArray("org0", "org3"),
		toByteArray("id0", "id3"),
	)}
	require.ErrorContains(t, nsVerifier.VerifyNs(fakeTxID, tx1, 0), "signature set did not satisfy policy")
}

func toByteArray(items ...string) [][]byte {
	itemBytes := make([][]byte, len(items))
	for i, it := range items {
		itemBytes[i] = []byte(it)
	}
	return itemBytes
}
