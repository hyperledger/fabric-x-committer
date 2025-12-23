/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package signature_test

import (
	"encoding/hex"
	"testing"

	fmsp "github.com/hyperledger/fabric-protos-go-apiv2/msp"
	"github.com/hyperledger/fabric-x-common/common/cauthdsl"
	"github.com/hyperledger/fabric-x-common/common/policydsl"
	"github.com/hyperledger/fabric-x-common/msp"
	"github.com/hyperledger/fabric-x-common/protoutil"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-committer/api/applicationpb"
	"github.com/hyperledger/fabric-x-committer/service/verifier/policy"
	"github.com/hyperledger/fabric-x-committer/utils/signature"
	"github.com/hyperledger/fabric-x-committer/utils/signature/sigtest"
	"github.com/hyperledger/fabric-x-committer/utils/test"
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
}

func TestNsVerifierSignatureRule(t *testing.T) {
	t.Parallel()
	mspIDs := []string{"org0", "org1", "org2", "org3"}
	certBytes := []string{"id0", "id1", "id2", "id3"}
	identities := make([][]byte, len(mspIDs))
	knownIdentities := make(map[msp.IdentityIdentifier]msp.Identity)
	for i, mspID := range mspIDs {
		identities[i] = protoutil.MarshalOrPanic(&fmsp.SerializedIdentity{Mspid: mspID, IdBytes: []byte(certBytes[i])})
		idIdentifier := msp.IdentityIdentifier{Mspid: mspID, Id: hex.EncodeToString([]byte(certBytes[i]))}
		knownIdentities[idIdentifier] = &cauthdsl.MockIdentity{MspID: mspID, IDBytes: []byte(certBytes[i])}
	}

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
		&applicationpb.NamespacePolicy{Rule: &applicationpb.NamespacePolicy_MspRule{MspRule: pBytes}},
		&cauthdsl.MockIdentityDeserializer{KnownIdentities: knownIdentities})
	require.NoError(t, err)

	for _, creatorType := range []int{test.CreatorCertificate, test.CreatorID} {
		tx1 := &applicationpb.Tx{
			Namespaces: []*applicationpb.TxNamespace{{
				NsId:       "1",
				NsVersion:  1,
				ReadWrites: []*applicationpb.ReadWrite{{Key: []byte("k1"), Value: []byte("v1")}},
			}},
		}
		// org0, org3, and org1 sign.
		tx1.Endorsements = []*applicationpb.Endorsements{sigtest.CreateEndorsementsForSignatureRule(
			toByteArray("s0", "s3", "s1"),
			toByteArray("org0", "org3", "org1"),
			toByteArray("id0", "id3", "id1"),
			creatorType,
		)}
		require.NoError(t, nsVerifier.VerifyNs(fakeTxID, tx1, 0))

		// org0, org3, and org2 sign.
		tx1.Endorsements = []*applicationpb.Endorsements{sigtest.CreateEndorsementsForSignatureRule(
			toByteArray("s0", "s3", "s2"),
			toByteArray("org0", "org3", "org2"),
			toByteArray("id0", "id3", "id2"),
			creatorType,
		)}
		require.NoError(t, nsVerifier.VerifyNs(fakeTxID, tx1, 0))

		tx1.Endorsements = []*applicationpb.Endorsements{sigtest.CreateEndorsementsForSignatureRule(
			toByteArray("s0", "s3"),
			toByteArray("org0", "org3"),
			toByteArray("id0", "id3"),
			creatorType,
		)}
		require.ErrorContains(t, nsVerifier.VerifyNs(fakeTxID, tx1, 0), "signature set did not satisfy policy")
	}
}

func toByteArray(items ...string) [][]byte {
	itemBytes := make([][]byte, len(items))
	for i, it := range items {
		itemBytes[i] = []byte(it)
	}
	return itemBytes
}
