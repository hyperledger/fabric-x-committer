/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package signature

import (
	"crypto/sha256"
	"strings"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-x-common/common/cauthdsl"
	"github.com/hyperledger/fabric-x-common/common/policies"
	"github.com/hyperledger/fabric-x-common/msp"
	"github.com/hyperledger/fabric-x-common/protoutil"

	"github.com/hyperledger/fabric-x-committer/api/applicationpb"
)

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
	signedData := make([]*protoutil.SignedData, len(endorsements))
	for i, s := range endorsements {
		idBytes, err := serializeIdentity(s.Identity, v.identityDeserializer)
		if err != nil {
			return err
		}
		signedData[i] = &protoutil.SignedData{
			Data:      data,
			Identity:  idBytes,
			Signature: s.Endorsement,
		}
	}
	return v.EvaluateSignedData(signedData)
}

func serializeIdentity(id *applicationpb.Identity, idDeserializer msp.IdentityDeserializer) ([]byte, error) {
	var idBytes []byte
	var err error
	switch creator := id.Creator.(type) {
	case *applicationpb.Identity_Certificate:
		if creator.Certificate == nil {
			return idBytes, errors.New("An empty certificate is provided for the identity")
		}
		idBytes, err = msp.NewSerializedIdentity(id.MspId, creator.Certificate)
	case *applicationpb.Identity_CertificateId:
		if creator.CertificateId == "" {
			return idBytes, errors.New("An empty certificate ID is provided for the identity")
		}
		identity := idDeserializer.GetKnownDeserializedIdentity(msp.IdentityIdentifier{
			Mspid: id.MspId,
			Id:    creator.CertificateId,
		})
		if identity == nil {
			return idBytes, errors.Newf("Invalid certificate identity: %s", creator.CertificateId)
		}
		idBytes, err = identity.Serialize()
	}
	return idBytes, errors.Wrap(err, "Identity serialization error")
}
