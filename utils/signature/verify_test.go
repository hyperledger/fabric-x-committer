/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package signature_test

import (
	"encoding/hex"
	"testing"

	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/hyperledger/fabric-x-common/api/msppb"
	"github.com/hyperledger/fabric-x-common/common/cauthdsl"
	"github.com/hyperledger/fabric-x-common/common/policydsl"
	"github.com/hyperledger/fabric-x-common/msp"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-committer/service/verifier/policy"
	"github.com/hyperledger/fabric-x-committer/utils/signature"
	"github.com/hyperledger/fabric-x-committer/utils/test"
	"github.com/hyperledger/fabric-x-committer/utils/testsig"
)

const fakeTxID = "fake-id"

func TestNsVerifierThresholdRule(t *testing.T) {
	t.Parallel()
	pItem, nsEndorser := policy.MakePolicyAndNsEndorser(t, "1")
	pol := &applicationpb.NamespacePolicy{}
	require.NoError(t, proto.Unmarshal(pItem.Policy, pol))
	nsVerifier, err := signature.NewNsVerifier(pol, nil)
	require.NoError(t, err)

	tx1 := &applicationpb.Tx{
		Namespaces: []*applicationpb.TxNamespace{{
			NsId:       "1",
			NsVersion:  1,
			ReadWrites: []*applicationpb.ReadWrite{{Key: []byte("k1"), Value: []byte("v1")}},
		}},
	}
	endorsement, err := nsEndorser.EndorseTxNs(fakeTxID, tx1, 0)
	require.NoError(t, err)
	tx1.Endorsements = []*applicationpb.Endorsements{endorsement}
	require.NoError(t, nsVerifier.VerifyNs(fakeTxID, tx1, 0))

	tx1.Endorsements = []*applicationpb.Endorsements{{
		EndorsementsWithIdentity: []*applicationpb.EndorsementWithIdentity{},
	}}
	err = nsVerifier.VerifyNs(fakeTxID, tx1, 0)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no endorsements provided")
}

func TestNsVerifierInvalidIdentities(t *testing.T) {
	t.Parallel()
	pItem, _ := policy.MakePolicyAndNsEndorser(t, "1")
	pol := &applicationpb.NamespacePolicy{}
	require.NoError(t, proto.Unmarshal(pItem.Policy, pol))
	nsVerifier, err := signature.NewNsVerifier(pol, nil)
	require.NoError(t, err)

	for _, tc := range []struct {
		name     string
		identity *msppb.Identity
	}{
		{name: "nil identity", identity: nil},
		{name: "empty identity struct", identity: &msppb.Identity{}},
		{name: "identity with only MspId", identity: &msppb.Identity{MspId: "hello"}},
		{name: "identity with unknown MspId and cert", identity: msppb.NewIdentity("hello", []byte("rand"))},
		{name: "identity with empty MspId and cert", identity: msppb.NewIdentity("", []byte("rand"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tx := &applicationpb.Tx{
				Namespaces: []*applicationpb.TxNamespace{{
					NsId:       "1",
					NsVersion:  1,
					ReadWrites: []*applicationpb.ReadWrite{{Key: []byte("k1"), Value: []byte("v1")}},
				}},
				Endorsements: []*applicationpb.Endorsements{{
					EndorsementsWithIdentity: []*applicationpb.EndorsementWithIdentity{{
						Endorsement: []byte("sign"),
						Identity:    tc.identity,
					}},
				}},
			}
			err := nsVerifier.VerifyNs(fakeTxID, tx, 0)
			require.Error(t, err)
			require.Contains(t, err.Error(), "signature mismatch")
		})
	}
}

func TestNsVerifierPolicyBased(t *testing.T) {
	t.Parallel()

	// Shared MSP identity setup used by both construction paths.
	mspID := "org0"
	certID := "id0"
	identity, err := msp.NewSerializedIdentity(mspID, []byte(certID))
	require.NoError(t, err)

	pol := policydsl.Envelope(policydsl.SignedBy(0), [][]byte{identity})
	pBytes, err := proto.Marshal(pol)
	require.NoError(t, err)

	idIdentifier := msp.IdentityIdentifier{Mspid: mspID, Id: hex.EncodeToString([]byte(certID))}
	deserializer := &cauthdsl.MockIdentityDeserializer{
		KnownIdentities: map[msp.IdentityIdentifier]msp.Identity{
			idIdentifier: &cauthdsl.MockIdentity{MspID: mspID, IDBytes: []byte(certID)},
		},
	}

	makeTx := func(endorsements []*applicationpb.Endorsements) *applicationpb.Tx {
		return &applicationpb.Tx{
			Namespaces: []*applicationpb.TxNamespace{{
				NsId:       "ns1",
				NsVersion:  1,
				ReadWrites: []*applicationpb.ReadWrite{{Key: []byte("k1"), Value: []byte("v1")}},
			}},
			Endorsements: endorsements,
		}
	}

	validEndorsements := func(creatorType int) []*applicationpb.Endorsements {
		return []*applicationpb.Endorsements{testsig.CreateEndorsementsForSignatureRule(
			toByteArray("sig0"),
			toByteArray(mspID),
			toByteArray(certID),
			creatorType,
		)}
	}

	unknownIdentity, err := msp.NewSerializedIdentity("unknown-msp", []byte("unknown-cert"))
	require.NoError(t, err)
	invalidEndorsements := []*applicationpb.Endorsements{testsig.CreateEndorsementsForSignatureRule(
		toByteArray("bad-sig"),
		toByteArray("unknown-msp"),
		[][]byte{unknownIdentity},
		test.CreatorCertificate,
	)}

	t.Run("from MSP rule", func(t *testing.T) {
		t.Parallel()
		verifier, errNs := signature.NewNsVerifier(
			&applicationpb.NamespacePolicy{Rule: &applicationpb.NamespacePolicy_MspRule{MspRule: pBytes}},
			deserializer)
		require.NoError(t, errNs)

		for _, creatorType := range []int{test.CreatorCertificate, test.CreatorID} {
			require.NoError(t, verifier.VerifyNs(fakeTxID, makeTx(validEndorsements(creatorType)), 0))
		}
		require.Error(t, verifier.VerifyNs(fakeTxID, makeTx(invalidEndorsements), 0))
	})

	t.Run("from channel policy", func(t *testing.T) {
		t.Parallel()
		pp := cauthdsl.NewPolicyProvider(deserializer)
		resolvedPolicy, _, errP := pp.NewPolicy(pBytes)
		require.NoError(t, errP)

		verifier := signature.NewNsVerifierFromChannelPolicy(resolvedPolicy)
		require.NotNil(t, verifier)

		for _, creatorType := range []int{test.CreatorCertificate, test.CreatorID} {
			require.NoError(t, verifier.VerifyNs(fakeTxID, makeTx(validEndorsements(creatorType)), 0))
		}
		require.NoError(t, verifier.UpdateIdentities(nil))
		require.Error(t, verifier.VerifyNs(fakeTxID, makeTx(invalidEndorsements), 0))
	})

	t.Run("nested MSP policy", func(t *testing.T) {
		t.Parallel()
		mspIDs := []string{"org0", "org1", "org2", "org3"}
		certs := []string{"id0", "id1", "id2", "id3"}
		identities := make([][]byte, len(mspIDs))
		knownIdentities := make(map[msp.IdentityIdentifier]msp.Identity)
		for i, id := range mspIDs {
			identities[i], err = msp.NewSerializedIdentity(id, []byte(certs[i]))
			require.NoError(t, err)
			idID := msp.IdentityIdentifier{Mspid: id, Id: hex.EncodeToString([]byte(certs[i]))}
			knownIdentities[idID] = &cauthdsl.MockIdentity{MspID: id, IDBytes: []byte(certs[i])}
		}

		// org0 and org3 must sign along with either org1 or org2. To realize this condition, the policy can be
		// written in many ways but we choose the following to test the nested structure.
		nested := policydsl.Envelope(
			policydsl.And(
				policydsl.Or(
					policydsl.And(policydsl.SignedBy(0), policydsl.SignedBy(1)),
					policydsl.And(policydsl.SignedBy(0), policydsl.SignedBy(2)),
				),
				policydsl.SignedBy(3),
			), identities)
		nestedBytes, err := proto.Marshal(nested)
		require.NoError(t, err)

		verifier, err := signature.NewNsVerifier(
			&applicationpb.NamespacePolicy{Rule: &applicationpb.NamespacePolicy_MspRule{MspRule: nestedBytes}},
			&cauthdsl.MockIdentityDeserializer{KnownIdentities: knownIdentities})
		require.NoError(t, err)

		for _, creatorType := range []int{test.CreatorCertificate, test.CreatorID} {
			// org0, org3, and org1 sign — satisfies first OR branch.
			require.NoError(t, verifier.VerifyNs(fakeTxID, makeTx([]*applicationpb.Endorsements{
				testsig.CreateEndorsementsForSignatureRule(
					toByteArray("s0", "s3", "s1"),
					toByteArray("org0", "org3", "org1"),
					toByteArray("id0", "id3", "id1"),
					creatorType,
				),
			}), 0))

			// org0, org3, and org2 sign — satisfies second OR branch.
			require.NoError(t, verifier.VerifyNs(fakeTxID, makeTx([]*applicationpb.Endorsements{
				testsig.CreateEndorsementsForSignatureRule(
					toByteArray("s0", "s3", "s2"),
					toByteArray("org0", "org3", "org2"),
					toByteArray("id0", "id3", "id2"),
					creatorType,
				),
			}), 0))

			// org0 and org3 only — missing org1 or org2.
			require.ErrorContains(t, verifier.VerifyNs(fakeTxID, makeTx([]*applicationpb.Endorsements{
				testsig.CreateEndorsementsForSignatureRule(
					toByteArray("s0", "s3"),
					toByteArray("org0", "org3"),
					toByteArray("id0", "id3"),
					creatorType,
				),
			}), 0), "signature set did not satisfy policy")
		}
	})
}

func toByteArray(items ...string) [][]byte {
	itemBytes := make([][]byte, len(items))
	for i, it := range items {
		itemBytes[i] = []byte(it)
	}
	return itemBytes
}
