/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package policy

import (
	"fmt"
	"testing"

	"github.com/hyperledger/fabric-x-common/protoutil"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
	"github.com/hyperledger/fabric-x-committer/api/types"
	"github.com/hyperledger/fabric-x-committer/utils/signature"
	"github.com/hyperledger/fabric-x-committer/utils/signature/sigtest"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

func TestGetUpdatesFromNamespace(t *testing.T) {
	t.Parallel()
	t.Log("meta namespace")
	items := make([]*protoblocktx.ReadWrite, 5)
	for i := range items {
		items[i] = &protoblocktx.ReadWrite{
			Key: fmt.Appendf(nil, "key-%d", i),
			Value: protoutil.MarshalOrPanic(&protoblocktx.NamespacePolicy{
				Rule: &protoblocktx.NamespacePolicy_ThresholdRule{
					ThresholdRule: &protoblocktx.ThresholdRule{
						Scheme: signature.Ecdsa, PublicKey: fmt.Appendf(nil, "value-%d", i),
					},
				},
			}),
		}
	}
	tx := &protoblocktx.TxNamespace{
		NsId:       types.MetaNamespaceID,
		ReadWrites: items,
	}
	update, err := GetUpdatesFromNamespace(tx)
	require.NoError(t, err)
	require.NotNil(t, update)
	require.NotNil(t, update.NamespacePolicies)
	require.Nil(t, update.Config)
	require.Len(t, update.NamespacePolicies.Policies, len(items))
	for i, p := range update.NamespacePolicies.Policies {
		require.Equal(t, items[i].Key, []byte(p.Namespace))
		nsPolicy := &protoblocktx.NamespacePolicy{}
		require.NoError(t, proto.Unmarshal(items[i].Value, nsPolicy))
		test.RequireProtoEqual(t, nsPolicy, p.Policy)
	}

	t.Log("config namespace")

	expectedValue := []byte("test config")
	tx = &protoblocktx.TxNamespace{
		NsId: types.ConfigKey,
		BlindWrites: []*protoblocktx.Write{{
			Key:   []byte(types.ConfigKey),
			Value: expectedValue,
		}},
	}

	update, err = GetUpdatesFromNamespace(tx)
	require.NoError(t, err)
	require.NotNil(t, update)
	require.NotNil(t, update.Config)
	require.Nil(t, update.NamespacePolicies)
	require.Equal(t, expectedValue, update.Config.Envelope)
}

func TestParsePolicyItem(t *testing.T) {
	t.Parallel()
	_, verificationKey := sigtest.NewSignatureFactory(signature.Ecdsa).NewKeys()
	p := MakeECDSAThresholdRuleNsPolicy(verificationKey)

	for _, ns := range []string{"0", "1"} {
		t.Run(fmt.Sprintf("valid policy ns: '%s'", ns), func(t *testing.T) {
			pd := MakePolicy(t, ns, p)
			retP, err := CreateNamespaceVerifier(pd, nil)
			require.NoError(t, err)
			require.Equal(t, p, retP.NamespacePolicy)
		})
	}

	for _, ns := range []string{
		"x", "abc_d", "a5_9z",
		"not_too_long_namespace_namespace_id_0123456789_0123456789_01",
	} {
		t.Run(fmt.Sprintf("valid ns: '%s'", ns), func(t *testing.T) {
			pd := MakePolicy(t, ns, p)
			_, err := CreateNamespaceVerifier(pd, nil)
			require.NoError(t, err)
		})
	}

	for _, ns := range []string{
		"", "abc_$", "a-", "go!", "My Namespace", "my name", "ABC_D", "new\nline",
		"____too_long_namespace_namespace_id_0123456789_0123456789_012",
		types.MetaNamespaceID, types.ConfigNamespaceID,
	} {
		t.Run(fmt.Sprintf("invalid ns: '%s'", ns), func(t *testing.T) {
			t.Parallel()
			pd := MakePolicy(t, ns, p)
			_, err := CreateNamespaceVerifier(pd, nil)
			require.ErrorIs(t, err, ErrInvalidNamespaceID)
		})
	}
}
