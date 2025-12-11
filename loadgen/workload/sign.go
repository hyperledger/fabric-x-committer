/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package workload

import (
	"maps"
	"os"
	"slices"

	"github.com/cockroachdb/errors"

	"github.com/hyperledger/fabric-x-committer/api/applicationpb"
	"github.com/hyperledger/fabric-x-committer/api/committerpb"
	"github.com/hyperledger/fabric-x-committer/utils"
	"github.com/hyperledger/fabric-x-committer/utils/logging"
	"github.com/hyperledger/fabric-x-committer/utils/signature"
	"github.com/hyperledger/fabric-x-committer/utils/signature/sigtest"
)

var logger = logging.New("load-gen-sign")

type (
	// TxEndorserVerifier supports endorsing and verifying a TX, given an endorser.
	TxEndorserVerifier struct {
		policies map[string]*NsPolicyEndorserVerifier
	}

	// NsPolicyEndorserVerifier supports endorsing and verifying a TX namespace.
	NsPolicyEndorserVerifier struct {
		endorser           *sigtest.NsEndorser
		verifier           *signature.NsVerifier
		verificationPolicy *applicationpb.NamespacePolicy
	}
)

var defaultPolicy = Policy{
	Scheme: signature.Ecdsa,
}

// NewTxEndorserVerifier creates a new TxEndorserVerifier given a workload profile.
func NewTxEndorserVerifier(policy *PolicyProfile) *TxEndorserVerifier {
	endorsers := make(map[string]*NsPolicyEndorserVerifier, len(policy.NamespacePolicies)+2)
	for nsID, p := range policy.NamespacePolicies {
		endorsers[nsID] = NewPolicyEndorserVerifier(p)
	}

	// We set default policy to ensure smooth operation even if the user did not specify anything.
	for _, nsID := range []string{DefaultGeneratedNamespaceID, committerpb.MetaNamespaceID} {
		if _, ok := endorsers[nsID]; !ok {
			endorsers[nsID] = NewPolicyEndorserVerifier(&defaultPolicy)
		}
	}

	return &TxEndorserVerifier{policies: endorsers}
}

// AllNamespaces returns all the endorser's supported namespaces.
func (e *TxEndorserVerifier) AllNamespaces() []string {
	return slices.Collect(maps.Keys(e.policies))
}

// Policy returns a namespace policy. It creates one if it does not exist.
func (e *TxEndorserVerifier) Policy(nsID string) *NsPolicyEndorserVerifier {
	policyEndorser, ok := e.policies[nsID]
	if !ok {
		policyEndorser = NewPolicyEndorserVerifier(&defaultPolicy)
		e.policies[nsID] = policyEndorser
	}
	return policyEndorser
}

// Endorse a TX.
func (e *TxEndorserVerifier) Endorse(txID string, tx *applicationpb.Tx) {
	tx.Endorsements = make([]*applicationpb.Endorsements, len(tx.Namespaces))
	for nsIndex, ns := range tx.Namespaces {
		signer, ok := e.policies[ns.NsId]
		if !ok {
			continue
		}
		tx.Endorsements[nsIndex] = signer.Endorse(txID, tx, nsIndex)
	}
}

// Verify an endorsement on the transaction.
func (e *TxEndorserVerifier) Verify(txID string, tx *applicationpb.Tx) bool {
	if len(tx.Endorsements) < len(tx.Namespaces) {
		return false
	}

	for nsIndex, ns := range tx.Namespaces {
		signer, ok := e.policies[ns.NsId]
		if !ok || !signer.Verify(txID, tx, nsIndex) {
			return false
		}
	}

	return true
}

// NewPolicyEndorserVerifier creates a new NsPolicyEndorserVerifier given a workload profile and a seed.
func NewPolicyEndorserVerifier(profile *Policy) *NsPolicyEndorserVerifier {
	logger.Debugf("sig profile: %v", profile)
	factory := sigtest.NewSignatureFactory(profile.Scheme)

	var signingKey signature.PrivateKey
	var verificationKey signature.PublicKey
	if profile.KeyPath != nil {
		logger.Infof("Attempting to load keys")
		var err error
		signingKey, verificationKey, err = loadKeys(*profile.KeyPath)
		utils.Must(err)
	} else {
		logger.Debugf("Generating new keys")
		signingKey, verificationKey = factory.NewKeysWithSeed(profile.Seed)
	}
	v, err := factory.NewVerifier(verificationKey)
	utils.Must(err)
	endorser, err := factory.NewEndorser(signingKey)
	utils.Must(err)

	return &NsPolicyEndorserVerifier{
		endorser: endorser,
		verifier: v,
		verificationPolicy: &applicationpb.NamespacePolicy{
			Rule: &applicationpb.NamespacePolicy_ThresholdRule{
				ThresholdRule: &applicationpb.ThresholdRule{
					Scheme: profile.Scheme, PublicKey: verificationKey,
				},
			},
		},
	}
}

// Endorse a TX.
func (e *NsPolicyEndorserVerifier) Endorse(txID string, tx *applicationpb.Tx, nsIndex int) *applicationpb.Endorsements {
	sign, err := e.endorser.EndorseTxNs(txID, tx, nsIndex)
	Must(err)
	return sign
}

// Verify a TX endorsement.
func (e *NsPolicyEndorserVerifier) Verify(txID string, tx *applicationpb.Tx, nsIndex int) bool {
	if err := e.verifier.VerifyNs(txID, tx, nsIndex); err != nil {
		return false
	}
	return true
}

// VerificationPolicy returns the verification policy.
func (e *NsPolicyEndorserVerifier) VerificationPolicy() *applicationpb.NamespacePolicy {
	return e.verificationPolicy
}

func loadKeys(keyPath KeyPath) (signingKey signature.PrivateKey, verificationKey signature.PublicKey, err error) {
	signingKey, err = os.ReadFile(keyPath.SigningKey)
	if err != nil {
		return nil, nil, errors.Wrapf(err,
			"could not read private key from %s", keyPath.SigningKey,
		)
	}

	if keyPath.VerificationKey != "" && utils.FileExists(keyPath.VerificationKey) {
		verificationKey, err = os.ReadFile(keyPath.VerificationKey)
		if err != nil {
			return nil, nil, errors.Wrapf(err,
				"could not read public key from %s", keyPath.VerificationKey,
			)
		}
		logger.Infof("Loaded private key and verification key from files %s and %s.",
			keyPath.SigningKey, keyPath.VerificationKey)
		return signingKey, verificationKey, nil
	}

	if keyPath.SignCertificate != "" && utils.FileExists(keyPath.SignCertificate) {
		verificationKey, err = sigtest.GetSerializedKeyFromCert(keyPath.SignCertificate)
		if err != nil {
			return nil, nil, errors.Wrapf(err,
				"could not read sign cert from %s", keyPath.SignCertificate,
			)
		}
		logger.Infof("Sign cert and key found in files %s/%s. Importing...",
			keyPath.SignCertificate, keyPath.SigningKey)
		return signingKey, verificationKey, nil
	}

	return nil, nil, errors.New("could not find verification key or sign certificate")
}
