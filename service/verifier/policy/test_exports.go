/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package policy

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
	"github.com/hyperledger/fabric-x-committer/utils/signature"
	"github.com/hyperledger/fabric-x-committer/utils/signature/sigtest"
)

// MakePolicy generates a policy item from a namespace policy.
func MakePolicy(
	t *testing.T,
	ns string,
	nsPolicy *protoblocktx.NamespacePolicy,
) *protoblocktx.PolicyItem {
	t.Helper()
	nsPolicyBytes, err := proto.Marshal(nsPolicy)
	require.NoError(t, err)
	return &protoblocktx.PolicyItem{
		Namespace: ns,
		Policy:    nsPolicyBytes,
	}
}

// MakePolicyAndNsSigner generates a policyItem and NsSigner.
func MakePolicyAndNsSigner(
	t *testing.T,
	ns string,
) (*protoblocktx.PolicyItem, *sigtest.NsSigner) {
	t.Helper()
	factory := sigtest.NewSignatureFactory(signature.Ecdsa)
	signingKey, verificationKey := factory.NewKeys()
	txSigner, err := factory.NewSigner(signingKey)
	require.NoError(t, err)
	p := MakePolicy(t, ns, MakeECDSAThresholdRuleNsPolicy(verificationKey))
	return p, txSigner
}

// MakeECDSAThresholdRuleNsPolicy generates a namespace policy with threshold rule.
func MakeECDSAThresholdRuleNsPolicy(publicKey []byte) *protoblocktx.NamespacePolicy {
	return &protoblocktx.NamespacePolicy{
		Rule: &protoblocktx.NamespacePolicy_ThresholdRule{
			ThresholdRule: &protoblocktx.ThresholdRule{
				Scheme: signature.Ecdsa, PublicKey: publicKey,
			},
		},
	}
}
