/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package signature

import (
	"crypto/sha256"
	"strings"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/hyperledger/fabric-x-common/common/cauthdsl"
	"github.com/hyperledger/fabric-x-common/common/policies"
	"github.com/hyperledger/fabric-x-common/msp"
	"github.com/hyperledger/fabric-x-common/protoutil"
)

// TODO: Move channelPolicyVerifier, mspPolicyVerifier, and keyVerifier to their own files.
//       It is difficult to read the current file due to the interleaving of methods from different objects.

type (
	// NsVerifier verifies a given namespace.
	NsVerifier struct {
		verifier
	}

	// verifier evaluates signatures and supports updating the identities.
	verifier interface {
		Verify(data []byte, endorsements []*applicationpb.EndorsementWithIdentity) error
		UpdateIdentities(idDeserializer msp.IdentityDeserializer) error
	}

	// channelPolicyVerifier verifies signatures using a pre-resolved channel policy (e.g., LifecycleEndorsement).
	// It could be combined with mspVerifier since both evaluate MSP-based policies, but UpdateIdentities
	// is not applicable here — the policy is already fully resolved from the channel config bundle.
	// Keeping it separate avoids adding no-op or conditional logic to mspVerifier.
	channelPolicyVerifier struct {
		policies.Policy
	}

	// mspVerifier verifies signatures based on an MSP policy.
	mspVerifier struct {
		policies.Policy
		mspRule              []byte
		identityDeserializer msp.IdentityDeserializer
	}

	// keyVerifier verifies signatures based on a public key.
	keyVerifier struct {
		digestVerifier
	}

	// digestVerifier verifies a signature against a digest.
	digestVerifier interface {
		verifyDigest(Digest, Signature) error
	}
)

// NewNsVerifier creates a new namespace verifier from a namespace policy.
func NewNsVerifier(p *applicationpb.NamespacePolicy, idDeserializer msp.IdentityDeserializer) (*NsVerifier, error) {
	switch r := p.Rule.(type) {
	case *applicationpb.NamespacePolicy_ThresholdRule:
		return NewNsVerifierFromKey(r.ThresholdRule.Scheme, r.ThresholdRule.PublicKey)
	case *applicationpb.NamespacePolicy_MspRule:
		return NewNsVerifierFromMsp(r.MspRule, idDeserializer)
	default:
		return nil, errors.Newf("policy rule '%v' not supported", p.Rule)
	}
}

// NewNsVerifierFromMsp creates a new namespace verifier from a msp rule.
func NewNsVerifierFromMsp(mspRule []byte, idDeserializer msp.IdentityDeserializer) (*NsVerifier, error) {
	v := &mspVerifier{mspRule: mspRule}
	err := v.UpdateIdentities(idDeserializer)
	if err != nil {
		return nil, err
	}
	return &NsVerifier{verifier: v}, nil
}

// NewNsVerifierFromChannelPolicy creates a new namespace verifier from a pre-resolved channel policy
// (e.g., LifecycleEndorsement).
func NewNsVerifierFromChannelPolicy(p policies.Policy) *NsVerifier {
	return &NsVerifier{verifier: &channelPolicyVerifier{Policy: p}}
}

// NewNsVerifierFromKey creates a new namespace verifier from a raw key and scheme.
func NewNsVerifierFromKey(scheme Scheme, key []byte) (*NsVerifier, error) {
	var err error
	var v verifier
	var dv digestVerifier
	switch strings.ToUpper(scheme) {
	case NoScheme, "":
		v = nil
	case Ecdsa:
		dv, err = newEcdsaVerifier(key)
		v = &keyVerifier{digestVerifier: dv}
	case Bls:
		dv, err = newBLSVerifier(key)
		v = &keyVerifier{digestVerifier: dv}
	case Eddsa:
		v = &keyVerifier{digestVerifier: &edDSAVerifier{PublicKey: key}}
	default:
		return nil, errors.Newf("scheme '%v' not supported", scheme)
	}
	return &NsVerifier{verifier: v}, err
}

// VerifyNs verifies a transaction's namespace signature.
func (v *NsVerifier) VerifyNs(txID string, tx *applicationpb.Tx, nsIndex int) error {
	if nsIndex < 0 || nsIndex >= len(tx.Namespaces) || nsIndex >= len(tx.Endorsements) {
		return errors.New("namespace index out of range")
	}
	if v.verifier == nil {
		return nil
	}

	data, err := tx.Namespaces[nsIndex].ASN1Marshal(txID)
	if err != nil {
		return err
	}
	return v.Verify(data, tx.Endorsements[nsIndex].EndorsementsWithIdentity)
}

// UpdateIdentities is nop for keyVerifier.
func (*keyVerifier) UpdateIdentities(msp.IdentityDeserializer) error {
	return nil
}

// Verify evaluates the endorsements against the data for keyVerifier.
func (v *keyVerifier) Verify(data []byte, endorsements []*applicationpb.EndorsementWithIdentity) error {
	digest := sha256.Sum256(data)
	return v.verifyDigest(digest[:], endorsements[0].Endorsement)
}

// UpdateIdentities updates the identities for mspVerifier.
func (v *mspVerifier) UpdateIdentities(idDeserializer msp.IdentityDeserializer) error {
	pp := cauthdsl.NewPolicyProvider(idDeserializer)
	newVerifier, _, err := pp.NewPolicy(v.mspRule)
	if err != nil {
		return errors.Wrap(err, "error updating msp verifier")
	}
	v.Policy = newVerifier
	v.identityDeserializer = idDeserializer
	return nil
}

// Verify evaluates the endorsements against the data for mspVerifier.
func (v *mspVerifier) Verify(data []byte, endorsements []*applicationpb.EndorsementWithIdentity) error {
	return evaluateEndorsements(v.Policy, data, endorsements)
}

// UpdateIdentities is a no-op for policyVerifier because the policy is pre-resolved from the bundle
// and replaced entirely on config updates.
func (*channelPolicyVerifier) UpdateIdentities(msp.IdentityDeserializer) error {
	return nil
}

// Verify evaluates the endorsements against the data for policyVerifier.
func (v *channelPolicyVerifier) Verify(data []byte, endorsements []*applicationpb.EndorsementWithIdentity) error {
	return evaluateEndorsements(v.Policy, data, endorsements)
}

// evaluateEndorsements constructs SignedData from endorsements and evaluates them against a policy.
func evaluateEndorsements(p policies.Policy, data []byte, endorsements []*applicationpb.EndorsementWithIdentity) error {
	signedData := make([]*protoutil.SignedData, len(endorsements))
	for i, s := range endorsements {
		signedData[i] = &protoutil.SignedData{
			Data:      data,
			Identity:  s.Identity,
			Signature: s.Endorsement,
		}
	}
	return p.EvaluateSignedData(signedData)
}
