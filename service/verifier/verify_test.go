/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package verifier

import (
	"testing"

	"github.com/hyperledger/fabric-lib-go/bccsp/factory"
	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/hyperledger/fabric-x-common/common/channelconfig"
	"github.com/hyperledger/fabric-x-common/protoutil"
	"github.com/hyperledger/fabric-x-common/utils/testcrypto"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/service/verifier/policy"
)

// TestCreateVerifiersSystemNamespaces guards createVerifiers's config-update branch:
// every namespace listed in policy.SystemNamespacePolicies must get its own freshly
// parsed verifier. A refactor that drops or skips an entry from that loop would not
// fail to compile -- the missing namespace would simply be absent from newVerifiers,
// and updatePolicies's carry-forward logic would silently keep serving the *previous*
// verifier for that namespace instead. This test catches that by name.
func TestCreateVerifiersSystemNamespaces(t *testing.T) {
	t.Parallel()

	configBlock, err := testcrypto.CreateOrExtendConfigBlockWithCrypto(t.TempDir(), &testcrypto.ConfigBlock{
		PeerOrganizationCount: 1,
	})
	require.NoError(t, err)

	envelope, err := protoutil.UnmarshalEnvelope(configBlock.Data.Data[0])
	require.NoError(t, err)
	bundle, err := channelconfig.NewBundleFromEnvelope(envelope, factory.GetDefault())
	require.NoError(t, err)

	update := &servicepb.VerifierUpdates{
		Config: &applicationpb.ConfigTransaction{
			Envelope: configBlock.Data.Data[0],
		},
	}

	newVerifiers, err := createVerifiers(update, bundle)
	require.NoError(t, err)

	require.Len(t, newVerifiers, len(policy.SystemNamespacePolicies))
	for _, snp := range policy.SystemNamespacePolicies {
		nsVerifier, ok := newVerifiers[snp.NamespaceID]
		require.True(t, ok, "expected a verifier for namespace %q", snp.NamespaceID)
		require.NotNil(t, nsVerifier)
	}
}
