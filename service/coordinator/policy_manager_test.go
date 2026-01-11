/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package coordinator

import (
	"testing"

	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/service/verifier/policy"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

func TestPolicyManager(t *testing.T) {
	t.Parallel()
	pm := newPolicyManager()

	t.Log("Initial state")
	update, version0 := pm.getAll()
	requireUpdateEqual(t, &servicepb.VerifierUpdates{
		NamespacePolicies: &applicationpb.NamespacePolicies{},
	}, update)

	t.Log("Update 1")
	ns1Policy := makeFakePolicy(t, "ns1", "k1")
	ns2Policy := makeFakePolicy(t, "ns2", "k2")
	update1 := &servicepb.VerifierUpdates{
		NamespacePolicies: &applicationpb.NamespacePolicies{
			Policies: []*applicationpb.PolicyItem{ns1Policy, ns2Policy},
		},
		Config: &applicationpb.ConfigTransaction{
			Envelope: []byte("config1"),
		},
	}
	pm.update(update1)

	update, version1u1 := pm.getUpdates(1)
	require.Nil(t, update)
	require.Greater(t, version1u1, version0)

	update, version1u0 := pm.getUpdates(0)
	requireUpdateEqual(t, update1, update)
	require.Equal(t, version1u1, version1u0)

	update, version1ua := pm.getAll()
	requireUpdateEqual(t, update1, update)
	require.Equal(t, version1u1, version1ua)

	t.Log("Update 2")
	ns2NewPolicy := makeFakePolicy(t, "ns2", "k3")
	update2 := &servicepb.VerifierUpdates{
		NamespacePolicies: &applicationpb.NamespacePolicies{
			Policies: []*applicationpb.PolicyItem{ns2NewPolicy},
		},
	}
	pm.update(update2)

	update, version2u2 := pm.getUpdates(2)
	require.Nil(t, update)
	require.Greater(t, version2u2, version1ua)

	update, version2u1 := pm.getUpdates(1)
	requireUpdateEqual(t, update2, update)
	require.Equal(t, version2u2, version2u1)

	update, version2u0 := pm.getUpdates(0)
	expectedAll := &servicepb.VerifierUpdates{
		NamespacePolicies: &applicationpb.NamespacePolicies{
			Policies: []*applicationpb.PolicyItem{ns1Policy, ns2NewPolicy},
		},
		Config: update1.Config,
	}
	requireUpdateEqual(t, expectedAll, update)
	require.Equal(t, version2u2, version2u0)

	update, version2ua := pm.getAll()
	requireUpdateEqual(t, expectedAll, update)
	require.Equal(t, version2u2, version2ua)

	t.Log("Update 3")
	update3 := &servicepb.VerifierUpdates{
		Config: &applicationpb.ConfigTransaction{
			Envelope: []byte("config2"),
		},
	}
	pm.update(update3)

	update, version3u3 := pm.getUpdates(3)
	require.Nil(t, update)
	require.Greater(t, version3u3, version2ua)

	update, version3u2 := pm.getUpdates(2)
	requireUpdateEqual(t, update3, update)
	require.Equal(t, version3u3, version3u2)

	update, version3u1 := pm.getUpdates(1)
	requireUpdateEqual(t, &servicepb.VerifierUpdates{
		NamespacePolicies: &applicationpb.NamespacePolicies{
			Policies: update2.NamespacePolicies.Policies,
		},
		Config: update3.Config,
	}, update)
	require.Equal(t, version3u3, version3u1)

	update, version3u0 := pm.getUpdates(0)
	expectedAll = &servicepb.VerifierUpdates{
		NamespacePolicies: &applicationpb.NamespacePolicies{
			Policies: []*applicationpb.PolicyItem{ns1Policy, ns2NewPolicy},
		},
		Config: update3.Config,
	}
	requireUpdateEqual(t, expectedAll, update)
	require.Equal(t, version3u3, version3u0)

	update, version3ua := pm.getAll()
	requireUpdateEqual(t, expectedAll, update)
	require.Equal(t, version3ua, version3u3)

	t.Log("Empty updates")
	pm.update(nil, &servicepb.VerifierUpdates{})
	_, versionNoUpdate := pm.getAll()
	require.Equal(t, version3u3, versionNoUpdate)
}

func requireUpdateEqual(t *testing.T, expected, actual *servicepb.VerifierUpdates) {
	t.Helper()
	if expected == nil {
		require.Nil(t, actual)
		return
	}
	require.NotNil(t, actual)
	if expected.Config != nil {
		require.NotNil(t, actual.Config)
		test.RequireProtoEqual(t, expected.Config, actual.Config)
	} else {
		require.Nil(t, actual.Config)
	}
	if expected.NamespacePolicies != nil {
		require.NotNil(t, actual.NamespacePolicies)
		// The policies may appear out of order due to the policy manager implementation.
		test.RequireProtoElementsMatch(t, actual.NamespacePolicies.Policies, expected.NamespacePolicies.Policies)
	} else {
		require.Nil(t, actual.NamespacePolicies)
	}
}

func makeFakePolicy(t *testing.T, ns, key string) *applicationpb.PolicyItem {
	t.Helper()
	return policy.MakePolicy(t, ns, policy.MakeECDSAThresholdRuleNsPolicy([]byte(key)))
}
