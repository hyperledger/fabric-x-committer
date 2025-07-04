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
	"github.com/hyperledger/fabric-x-committer/api/types"
	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
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
	var policyBytes []byte
	if ns == types.MetaNamespaceID {
		block, err := workload.CreateDefaultConfigBlock(&workload.ConfigBlock{
			MetaNamespaceVerificationKey: nsPolicy.PublicKey,
		})
		require.NoError(t, err)
		policyBytes = block.Data.Data[0]
	} else {
		pBytes, err := proto.Marshal(nsPolicy)
		require.NoError(t, err)
		policyBytes = pBytes
	}

	return &protoblocktx.PolicyItem{
		Namespace: ns,
		Policy:    policyBytes,
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
	p := MakePolicy(t, ns, &protoblocktx.NamespacePolicy{
		PublicKey: verificationKey,
		Scheme:    signature.Ecdsa,
	})
	return p, txSigner
}
