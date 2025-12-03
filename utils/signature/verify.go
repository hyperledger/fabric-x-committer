/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package signature

import (
	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-x-common/common/cauthdsl"
	"github.com/hyperledger/fabric-x-common/common/policies"
	"github.com/hyperledger/fabric-x-common/msp"
	"github.com/hyperledger/fabric-x-common/protoutil"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
)

// NsVerifier verifies a given namespace.
type NsVerifier struct {
	verifier        policies.Policy
	NamespacePolicy *protoblocktx.NamespacePolicy
	idDeserilizer   msp.IdentityDeserializer
}

// NewNsVerifier creates a new namespace verifier according to the implementation scheme.
func NewNsVerifier(p *protoblocktx.NamespacePolicy, idDeserializer msp.IdentityDeserializer) (*NsVerifier, error) {
	res := &NsVerifier{
		NamespacePolicy: p,
		idDeserilizer:   idDeserializer,
	}
	var err error

	switch r := p.GetRule().(type) {
	case *protoblocktx.NamespacePolicy_ThresholdRule:
		policy := r.ThresholdRule

		switch policy.Scheme {
		case NoScheme, "":
			res.verifier = nil
		case Ecdsa:
			res.verifier, err = newEcdsaVerifier(policy.PublicKey)
		case Bls:
			res.verifier, err = newBLSVerifier(policy.PublicKey)
		case Eddsa:
			res.verifier = &edDSAVerifier{PublicKey: policy.PublicKey}
		default:
			return nil, errors.Newf("scheme '%v' not supported", policy.Scheme)
		}
	case *protoblocktx.NamespacePolicy_MspRule:
		pp := cauthdsl.NewPolicyProvider(idDeserializer)
		res.verifier, _, err = pp.NewPolicy(r.MspRule)
	default:
		return nil, errors.Newf("policy rule '%v' not supported", p.GetRule())
	}
	return res, err
}

// VerifyNs verifies a transaction's namespace signature.
func (v *NsVerifier) VerifyNs(txID string, tx *protoblocktx.Tx, nsIndex int) error {
	if nsIndex < 0 || nsIndex >= len(tx.Namespaces) || nsIndex >= len(tx.Endorsements) {
		return errors.New("namespace index out of range")
	}

	if v.verifier == nil {
		return nil
	}

	data, err := ASN1MarshalTxNamespace(txID, tx.Namespaces[nsIndex])
	if err != nil {
		return err
	}

	endorsements := tx.Endorsements[nsIndex].EndorsementsWithIdentity
	signedData := make([]*protoutil.SignedData, 0, len(endorsements))

	switch v.NamespacePolicy.GetRule().(type) {
	case *protoblocktx.NamespacePolicy_ThresholdRule:
		signedData = append(signedData, &protoutil.SignedData{
			Data:      data,
			Signature: endorsements[0].Endorsement,
		})
	case *protoblocktx.NamespacePolicy_MspRule:
		for _, s := range endorsements {
			var idBytes []byte
			switch s.Identity.Creator.(type) {
			case *protoblocktx.Identity_Certificate:
				cert := s.Identity.GetCertificate()
				if cert == nil {
					return errors.New("An empty certificate is provided for the identity")
				}
				idBytes, err = msp.NewSerializedIdentity(s.Identity.MspId, cert)
				if err != nil {
					return err
				}
			case *protoblocktx.Identity_CertificateId:
				certID := s.Identity.GetCertificateId()
				if certID == "" {
					return errors.New("An empty certificate ID is provided for the identity")
				}

				identity := v.idDeserilizer.GetKnownDeserializedIdentity(msp.IdentityIdentifier{
					Mspid: s.Identity.MspId,
					Id:    certID,
				})
				if identity == nil {
					return errors.Newf("Invalid certificate identity: %s", certID)
				}

				idBytes, err = identity.Serialize()
				if err != nil {
					return errors.Wrapf(err, "invalid certificate identifier: %s", certID)
				}
			}

			signedData = append(signedData, &protoutil.SignedData{
				Data:      data,
				Identity:  idBytes,
				Signature: s.Endorsement,
			})
		}
	default:
		return errors.Newf("policy rule [%v] not supported", v.NamespacePolicy.GetRule())
	}

	return v.verifier.EvaluateSignedData(signedData)
}

// verifier verifies a digest.
type verifier interface {
	verify(Digest, Signature) error
}

func verifySignedData(signatureSet []*protoutil.SignedData, v verifier) error {
	for _, s := range signatureSet {
		if err := v.verify(digest(s.Data), s.Signature); err != nil {
			return err
		}
	}
	return nil
}
