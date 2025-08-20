/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package workload

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
	"github.com/hyperledger/fabric-x-committer/api/types"
	"github.com/hyperledger/fabric-x-committer/utils/signature"
)

var namespacesUnderTest = []string{GeneratedNamespaceID, "1", "2", types.MetaNamespaceID}

// TestNamespaceGeneratorKeyCreation verifies that the signers that created by the
// namespace-generator are the same given the same seed number.
func TestNamespaceGeneratorKeyCreation(t *testing.T) {
	t.Parallel()
	for _, scheme := range signature.AllRealSchemes {
		policyProfile := makePolicyProfile(scheme)
		t.Run(scheme, func(t *testing.T) {
			t.Parallel()
			mainNsTx, err := CreateLoadGenNamespacesTX(policyProfile)
			require.NoError(t, err)
			mainRw := getReadWritesFromNamespaceTx(t, mainNsTx.Tx)

			// Check for consistency.
			for range 1000 {
				nsTx, err2 := CreateLoadGenNamespacesTX(policyProfile)
				require.NoError(t, err2)
				curRw := getReadWritesFromNamespaceTx(t, nsTx.Tx)
				require.ElementsMatch(t, curRw, mainRw)
			}

			// changing seed for different key generation.
			for _, p := range policyProfile.NamespacePolicies {
				p.Seed += 100
			}
			nsTx, err := CreateLoadGenNamespacesTX(policyProfile)
			require.NoError(t, err)
			curRw := getReadWritesFromNamespaceTx(t, nsTx.Tx)
			for _, rw := range mainRw {
				for _, cRw := range curRw {
					require.NotEqual(t, rw, cRw)
				}
			}
		})
	}
}

func TestCreateConfigTx(t *testing.T) {
	t.Parallel()
	policyProfile := makePolicyProfile(signature.Ecdsa)
	tx, err := CreateConfigTx(policyProfile)
	require.NoError(t, err)
	require.NotNil(t, tx)
	require.NotEmpty(t, tx.Id)
	require.NotEmpty(t, tx.EnvelopePayload)
	require.NotEmpty(t, tx.SerializedEnvelope)
}

func getReadWritesFromNamespaceTx(t *testing.T, tx *protoblocktx.Tx) []*protoblocktx.ReadWrite {
	t.Helper()
	require.NotNil(t, tx)
	require.Len(t, tx.Namespaces, 1)
	require.Equal(t, types.MetaNamespaceID, tx.Namespaces[0].NsId)
	require.Len(t, tx.Namespaces[0].ReadWrites, len(namespacesUnderTest)-1)
	return tx.Namespaces[0].ReadWrites
}

func makePolicyProfile(schema signature.Scheme) *PolicyProfile {
	policyProfile := &PolicyProfile{
		NamespacePolicies: make(map[string]*Policy, len(namespacesUnderTest)),
	}
	for i, ns := range namespacesUnderTest {
		policyProfile.NamespacePolicies[ns] = &Policy{
			Scheme: schema,
			Seed:   int64(i),
		}
	}
	return policyProfile
}
