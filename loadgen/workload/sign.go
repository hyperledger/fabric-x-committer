/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package workload

import (
	"maps"
	"os"
	"strings"

	"github.com/hyperledger/fabric-lib-go/common/flogging"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/hyperledger/fabric-x-common/common/policydsl"
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

var defaultPolicy = Policy{Scheme: signature.Ecdsa}

// NewTxEndorser creates a new TxEndorser given a workload profile.
func NewTxEndorser(policy *PolicyProfile) *TxEndorser {
	// We set default policy to ensure smooth operation even if the user did not specify anything.
	nsPolicies := policy.NamespacePolicies
	for _, nsID := range []string{DefaultGeneratedNamespaceID} {
		if _, ok := nsPolicies[nsID]; !ok {
			nsPolicies[nsID] = &defaultPolicy
		}
	}
	endorsers := make(map[string]*testsig.NsEndorser, len(nsPolicies)+1)
	policies := make(map[string]*applicationpb.NamespacePolicy, len(nsPolicies)+1)
	for nsID, p := range nsPolicies {
		endorsers[nsID], policies[nsID] = newPolicyEndorser(policy.ArtifactsPath, p)
	}

	if policy.ArtifactsPath != "" {
		endorsers[committerpb.MetaNamespaceID], policies[committerpb.MetaNamespaceID] = newPolicyEndorser(
			policy.ArtifactsPath, &Policy{Scheme: "MSP"})
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
// given a workload profile and a seed.
func newPolicyEndorser(cryptoPath string, profile *Policy) (*testsig.NsEndorser, *applicationpb.NamespacePolicy) {
	if profile == nil {
		profile = &defaultPolicy
	}
	switch strings.ToUpper(profile.Scheme) {
	case "MSP":
		return NewPolicyEndorserFromMsp(cryptoPath)
	default:
		signingKey, verificationKey := getKeyPair(profile)
		return newPolicyEndorserFromKey(profile.Scheme, signingKey, verificationKey)
	}
}

// newPolicyEndorserFromKey creates a new [testsig.NsEndorser] and its [applicationpb.NamespacePolicy]
// given a scheme and a key pair.
func newPolicyEndorserFromKey(
	scheme string, signingKey, verificationKey []byte,
) (*testsig.NsEndorser, *applicationpb.NamespacePolicy) {
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

// NewPolicyEndorserFromMsp creates an MSP-based endorser and namespace policy from the
// peer organization crypto artifacts under artifactsPath.
func NewPolicyEndorserFromMsp(artifactsPath string) (*testsig.NsEndorser, *applicationpb.NamespacePolicy) {
	signingIdentities, err := testcrypto.GetPeersIdentities(artifactsPath)
	utils.Must(err)
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
