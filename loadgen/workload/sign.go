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
	"github.com/hyperledger/fabric-x-committer/utils/test/apptest"
)

var logger = logging.New("load-gen-sign")

type (
	// TxSignerVerifier supports signing and verifying a TX, given a hash signer.
	TxSignerVerifier struct {
		policies map[string]*PolicySignerVerifier
	}

	// PolicySignerVerifier supports signing and verifying a hash value.
	PolicySignerVerifier struct {
		signer             *sigtest.NsSigner
		verifier           *signature.NsVerifier
		verificationPolicy *applicationpb.NamespacePolicy
	}
)

var defaultPolicy = Policy{
	Scheme: signature.Ecdsa,
}

// NewTxSignerVerifier creates a new TxSignerVerifier given a workload profile.
func NewTxSignerVerifier(policy *PolicyProfile) *TxSignerVerifier {
	signers := make(map[string]*PolicySignerVerifier, len(policy.NamespacePolicies)+2)
	for nsID, p := range policy.NamespacePolicies {
		signers[nsID] = NewPolicySignerVerifier(p)
	}

	// We set default policy to ensure smooth operation even if the user did not specify anything.
	for _, ns := range []string{GeneratedNamespaceID, committerpb.MetaNamespaceID} {
		if _, ok := signers[ns]; !ok {
			signers[ns] = NewPolicySignerVerifier(&defaultPolicy)
		}
	}

	return &TxSignerVerifier{policies: signers}
}

// AllNamespaces returns all the signer's supported namespaces.
func (e *TxSignerVerifier) AllNamespaces() []string {
	return slices.Collect(maps.Keys(e.policies))
}

// Policy returns a namespace policy. It creates one if it does not exist.
func (e *TxSignerVerifier) Policy(nsID string) *PolicySignerVerifier {
	policySigner, ok := e.policies[nsID]
	if !ok {
		policySigner = NewPolicySignerVerifier(&defaultPolicy)
		e.policies[nsID] = policySigner
	}
	return policySigner
}

// Sign signs a TX.
func (e *TxSignerVerifier) Sign(txID string, tx *applicationpb.Tx) {
	tx.Endorsements = make([]*applicationpb.Endorsements, len(tx.Namespaces))
	for nsIndex, ns := range tx.Namespaces {
		signer, ok := e.policies[ns.NsId]
		if !ok {
			continue
		}
		tx.Endorsements[nsIndex] = apptest.CreateEndorsementsForThresholdRule(signer.Sign(txID, tx, nsIndex))[0]
	}
}

// Verify verifies a signature on the transaction.
func (e *TxSignerVerifier) Verify(txID string, tx *applicationpb.Tx) bool {
	if len(tx.Endorsements) < len(tx.Namespaces) {
		return false
	}

	for nsIndex, ns := range tx.GetNamespaces() {
		signer, ok := e.policies[ns.NsId]
		if !ok || !signer.Verify(txID, tx, nsIndex) {
			return false
		}
	}

	return true
}

// NewPolicySignerVerifier creates a new PolicySignerVerifier given a workload profile and a seed.
func NewPolicySignerVerifier(profile *Policy) *PolicySignerVerifier {
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
	signer, err := factory.NewSigner(signingKey)
	utils.Must(err)

	return &PolicySignerVerifier{
		signer:   signer,
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

// Sign signs a hash.
func (e *PolicySignerVerifier) Sign(txID string, tx *applicationpb.Tx, nsIndex int) signature.Signature {
	sign, err := e.signer.SignNs(txID, tx, nsIndex)
	Must(err)
	return sign
}

// Verify verifies a Signature.
func (e *PolicySignerVerifier) Verify(txID string, tx *applicationpb.Tx, nsIndex int) bool {
	if err := e.verifier.VerifyNs(txID, tx, nsIndex); err != nil {
		return false
	}
	return true
}

// VerificationPolicy returns the verification policy.
func (e *PolicySignerVerifier) VerificationPolicy() *applicationpb.NamespacePolicy {
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
