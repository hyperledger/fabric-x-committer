/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package policy

import (
	"fmt"
	"testing"

	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/hyperledger/fabric-x-common/protoutil"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/utils/signature"
	"github.com/hyperledger/fabric-x-committer/utils/signature/sigtest"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

func TestGetUpdatesFromNamespace(t *testing.T) {
	t.Parallel()
	t.Log("meta namespace")
	items := make([]*applicationpb.ReadWrite, 5)
	for i := range items {
		items[i] = &applicationpb.ReadWrite{
			Key:   fmt.Appendf(nil, "key-%d", i),
			Value: protoutil.MarshalOrPanic(MakeECDSAThresholdRuleNsPolicy(fmt.Appendf(nil, "value-%d", i))),
		}
	}
	tx := &applicationpb.TxNamespace{
		NsId:       committerpb.MetaNamespaceID,
		ReadWrites: items,
	}
	update := GetUpdatesFromNamespace(tx)
	require.NotNil(t, update.GetNamespacePolicies())
	require.Nil(t, update.Config)
	require.Len(t, update.NamespacePolicies.Policies, len(items))
	for i, p := range update.NamespacePolicies.Policies {
		require.Equal(t, items[i].Key, []byte(p.Namespace))
		require.Equal(t, items[i].Value, p.Policy)
	}

	t.Log("config namespace")

	expectedValue := []byte("test config")
	tx = &applicationpb.TxNamespace{
		NsId: committerpb.ConfigKey,
		BlindWrites: []*applicationpb.Write{{
			Key:   []byte(committerpb.ConfigKey),
			Value: expectedValue,
		}},
	}

	update = GetUpdatesFromNamespace(tx)
	require.NotNil(t, update)
	require.NotNil(t, update.Config)
	require.Nil(t, update.NamespacePolicies)
	require.Equal(t, expectedValue, update.Config.Envelope)
}

func TestParsePolicyItem(t *testing.T) {
	t.Parallel()
	_, verificationKey := sigtest.NewKeyPair(signature.Ecdsa)
	p := MakeECDSAThresholdRuleNsPolicy(verificationKey)

	for _, ns := range []string{"0", "1"} {
		t.Run(fmt.Sprintf("valid policy ns: '%s'", ns), func(t *testing.T) {
			pd := MakePolicy(t, ns, p)
			retP, err := CreateNamespaceVerifier(pd, nil)
			require.NoError(t, err)
			require.NotNil(t, retP)
			pol, err := UnmarshalNamespacePolicy(pd.Policy)
			require.NoError(t, err)
			test.RequireProtoEqual(t, p, pol)
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
		committerpb.MetaNamespaceID, committerpb.ConfigNamespaceID,
	} {
		t.Run(fmt.Sprintf("invalid ns: '%s'", ns), func(t *testing.T) {
			t.Parallel()
			pd := MakePolicy(t, ns, p)
			_, err := CreateNamespaceVerifier(pd, nil)
			require.ErrorIs(t, err, ErrInvalidNamespaceID)
		})
	}

	t.Run("invalid policy", func(t *testing.T) {
		pd := MakePolicy(t, "0", p)
		pd.Policy = protoutil.MarshalOrPanic(MakeECDSAThresholdRuleNsPolicy([]byte("bad-policy")))
		_, err := CreateNamespaceVerifier(pd, nil)
		require.Error(t, err)
	})
}
