/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package policy

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-committer/api/applicationpb"
	"github.com/hyperledger/fabric-x-committer/utils/signature"
	"github.com/hyperledger/fabric-x-committer/utils/signature/sigtest"
)

// MakePolicy generates a policy item from a namespace policy.
func MakePolicy(
	t *testing.T,
	ns string,
	nsPolicy *applicationpb.NamespacePolicy,
) *applicationpb.PolicyItem {
	t.Helper()
	nsPolicyBytes, err := proto.Marshal(nsPolicy)
	require.NoError(t, err)
	return &applicationpb.PolicyItem{
		Namespace: ns,
		Policy:    nsPolicyBytes,
	}
}

// MakePolicyAndNsEndorser generates a policyItem and TxNsEndorser.
func MakePolicyAndNsEndorser(
	t *testing.T,
	ns string,
) (*applicationpb.PolicyItem, *sigtest.TxNsEndorser) {
	t.Helper()
	signingKey, verificationKey := sigtest.NewKeys(signature.Ecdsa)
	txSigner, err := sigtest.NewRawKeyEndorser(signature.Ecdsa, signingKey)
	require.NoError(t, err)
	p := MakePolicy(t, ns, MakeECDSAThresholdRuleNsPolicy(verificationKey))
	return p, txSigner
}

// MakeECDSAThresholdRuleNsPolicy generates a namespace policy with threshold rule.
func MakeECDSAThresholdRuleNsPolicy(publicKey []byte) *applicationpb.NamespacePolicy {
	return &applicationpb.NamespacePolicy{
		Rule: &applicationpb.NamespacePolicy_ThresholdRule{
			ThresholdRule: &applicationpb.ThresholdRule{
				Scheme: signature.Ecdsa, PublicKey: publicKey,
			},
		},
	}
}
