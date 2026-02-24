/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package policy

import (
	"testing"

	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-committer/utils/signature"
	"github.com/hyperledger/fabric-x-committer/utils/testsig"
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

// MakePolicyAndNsEndorser generates a policyItem and NsEndorser.
func MakePolicyAndNsEndorser(
	t *testing.T,
	ns string,
) (*applicationpb.PolicyItem, *testsig.NsEndorser) {
	t.Helper()
	signingKey, verificationKey := testsig.NewKeyPair(signature.Ecdsa)
	txEndorser, err := testsig.NewNsEndorserFromKey(signature.Ecdsa, signingKey)
	require.NoError(t, err)
	p := MakePolicy(t, ns, MakeECDSAThresholdRuleNsPolicy(verificationKey))
	return p, txEndorser
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
