/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package workload

import (
	"maps"
	"os"

	"github.com/hyperledger/fabric-lib-go/common/flogging"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/hyperledger/fabric-x-common/common/policydsl"
	"github.com/hyperledger/fabric-x-common/msp"
	"github.com/hyperledger/fabric-x-common/protoutil"

	"github.com/hyperledger/fabric-x-committer/utils"
	"github.com/hyperledger/fabric-x-committer/utils/signature"
	"github.com/hyperledger/fabric-x-committer/utils/test"
	"github.com/hyperledger/fabric-x-committer/utils/testcrypto"
	"github.com/hyperledger/fabric-x-committer/utils/testsig"
)

var logger = flogging.MustGetLogger("load-gen-sign")

// TxEndorser supports endorsing a TX.
type TxEndorser struct {
	endorsers map[string]*testsig.NsEndorser
	policies  map[string]*applicationpb.NamespacePolicy
}

// NewTxEndorser creates a new TxEndorser given a workload profile.
func NewTxEndorser(policy *PolicyProfile) *TxEndorser {
	// We set default policy to ensure smooth operation even if the user did not specify anything.
	// We clone the map to avoid modifying the input policy as this can cause data races.
	nsPolicies := maps.Clone(policy.NamespacePolicies)
	for _, nsID := range []string{DefaultGeneratedNamespaceID, committerpb.MetaNamespaceID} {
		if _, ok := nsPolicies[nsID]; !ok {
			nsPolicies[nsID] = nil
		}
	}
	endorsers := make(map[string]*testsig.NsEndorser, len(nsPolicies))
	policies := make(map[string]*applicationpb.NamespacePolicy, len(nsPolicies))
	for nsID, p := range nsPolicies {
		endorsers[nsID], policies[nsID] = newPolicyEndorser(policy.ArtifactsPath, p)
	}

	return &TxEndorser{
		policies:  policies,
		endorsers: endorsers,
	}
}

// VerificationPolicies returns the verification policies.
func (e *TxEndorser) VerificationPolicies() map[string]*applicationpb.NamespacePolicy {
	return maps.Clone(e.policies)
}

// Endorse a TX.
func (e *TxEndorser) Endorse(txID string, tx *applicationpb.Tx) {
	tx.Endorsements = make([]*applicationpb.Endorsements, len(tx.Namespaces))
	for nsIndex, ns := range tx.Namespaces {
		signer, ok := e.endorsers[ns.NsId]
		if !ok {
			continue
		}
		endorsement, err := signer.EndorseTxNs(txID, tx, nsIndex)
		utils.Must(err)
		tx.Endorsements[nsIndex] = endorsement
	}
}

// newPolicyEndorser creates a new [testsig.NsEndorser] and its [applicationpb.NamespacePolicy]
// given a workload profile and a policy configuration.
//
// For MSP scheme:
//   - If policy.MSPIdentities is provided, uses those identities,
//   - Otherwise, loads identities from cryptoPath,
//   - If neither is available, creates an endorser with no identities.
//
// For other schemes (ECDSA, EDDSA, BLS):
//   - Generates or loads a key pair based on policy.Seed or policy.KeyPath.
func newPolicyEndorser(artifactsPath string, policy *Policy) (*testsig.NsEndorser, *applicationpb.NamespacePolicy) {
	if policy == nil {
		policy = &Policy{}
	}
	scheme := getPolicyScheme(policy)
	switch scheme {
	case PolicySchemeMSP:
		var mspDirectories []*msp.DirLoadParameters
		switch {
		case len(policy.MSPIdentities) > 0:
			mspDirectories = make([]*msp.DirLoadParameters, len(policy.MSPIdentities))
			for i, id := range policy.MSPIdentities {
				mspDirectories[i] = &msp.DirLoadParameters{MspName: id.MspID, MspDir: id.MSPDir, CspConf: id.BCCSP}
			}
		case len(artifactsPath) > 0:
			mspDirectories = testcrypto.GetPeersMspDirs(artifactsPath)
		default:
			logger.Warn("MSP scheme configured but no identities provided")
			// No identities. No endorsement required.
			// Valid for test scenarios where no endorsement is needed.
			// When evaluating production deployments, MSPIdentities or ArtifactsPath must be provided.
		}
		signingIdentities, err := testcrypto.GetSigningIdentities(mspDirectories...)
		utils.Must(err)
		return newPolicyEndorserFromMSP(signingIdentities)
	default:
		signingKey, verificationKey := getKeyPair(policy)
		endorser, err := testsig.NewNsEndorserFromKey(scheme, signingKey)
		utils.Must(err)
		nsPolicy := &applicationpb.NamespacePolicy{
			Rule: &applicationpb.NamespacePolicy_ThresholdRule{
				ThresholdRule: &applicationpb.ThresholdRule{
					Scheme: scheme, PublicKey: verificationKey,
				},
			},
		}
		return endorser, nsPolicy
	}
}

// newPolicyEndorserFromMSP creates an MSP-based endorser and namespace policy from the given identities.
func newPolicyEndorserFromMSP(signingIdentities []msp.SigningIdentity) (
	*testsig.NsEndorser, *applicationpb.NamespacePolicy,
) {
	endorser, err := testsig.NewNsEndorserFromMsp(test.CreatorID, signingIdentities...)
	utils.Must(err)

	serializedSigningIdentities := make([][]byte, len(signingIdentities))
	sigPolicies := make([]*common.SignaturePolicy, len(signingIdentities))
	for i, si := range signingIdentities {
		siBytes, serErr := si.Serialize()
		utils.Must(serErr)
		serializedSigningIdentities[i] = siBytes
		sigPolicies[i] = policydsl.SignedBy(int32(i)) //nolint:gosec // safe int -> int32.
	}

	nsPolicy := &applicationpb.NamespacePolicy{
		Rule: &applicationpb.NamespacePolicy_MspRule{
			MspRule: protoutil.MarshalOrPanic(
				policydsl.Envelope(
					//nolint:gosec // safe int -> int32.
					policydsl.NOutOf(int32(len(serializedSigningIdentities)), sigPolicies),
					serializedSigningIdentities,
				),
			),
		},
	}
	return endorser, nsPolicy
}

func getKeyPair(profile *Policy) (signingKey signature.PrivateKey, verificationKey signature.PublicKey) {
	if profile.KeyPath == nil {
		logger.Debugf("Generating new keys")
		return testsig.NewKeyPairWithSeed(profile.Scheme, profile.Seed)
	}

	k := profile.KeyPath
	var err error

	logger.Infof("Loading signing key from file '%s'.", k.SigningKey)
	signingKey, err = os.ReadFile(k.SigningKey)
	utils.Mustf(err, "could not read signing key from %s", k.SigningKey)

	switch {
	case k.VerificationKey != "" && utils.FileExists(k.VerificationKey):
		logger.Infof("Loading verification key from file '%s'.", k.VerificationKey)
		verificationKey, err = os.ReadFile(k.VerificationKey)
		utils.Mustf(err, "could not read public key from %s", k.VerificationKey)
	case k.SignCertificate != "" && utils.FileExists(k.SignCertificate):
		logger.Infof("Loading sign certiticate from file '%s'.", k.SignCertificate)
		verificationKey, err = testsig.GetSerializedKeyFromCert(k.SignCertificate)
		utils.Mustf(err, "could not read sign cert from %s", k.SignCertificate)
	default:
		panic("could not find verification key or sign certificate")
	}

	return signingKey, verificationKey
}
