/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package signature

import (
	"github.com/cockroachdb/errors"
	fmsp "github.com/hyperledger/fabric-protos-go-apiv2/msp"
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
}

// NewNsVerifier creates a new namespace verifier according to the implementation scheme.
func NewNsVerifier(p *protoblocktx.NamespacePolicy, idDeserializer msp.IdentityDeserializer) (*NsVerifier, error) {
	res := &NsVerifier{
		NamespacePolicy: p,
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
	case *protoblocktx.NamespacePolicy_SignatureRule:
		pp := cauthdsl.NewPolicyProvider(idDeserializer)
		res.verifier, _, err = pp.NewPolicy(r.SignatureRule)
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

	switch v.NamespacePolicy.GetRule().(type) {
	case *protoblocktx.NamespacePolicy_ThresholdRule:
		return v.verifier.EvaluateSignedData([]*protoutil.SignedData{
			{
				Data:      data,
				Signature: tx.Endorsements[nsIndex].EndorsementsWithIdentity[0].Endorsement,
			},
		})
	case *protoblocktx.NamespacePolicy_SignatureRule:
		signedData := make([]*protoutil.SignedData, len(tx.Endorsements[nsIndex].EndorsementsWithIdentity))
		for i, s := range tx.Endorsements[nsIndex].EndorsementsWithIdentity {
			// NOTE: CertificateID is not supported as MSP does not have the supported for pre-stored certificates yet.
			cert := s.Identity.GetCertificate()
			if cert == nil {
				return errors.New("An empty certificate is provided for the identity")
			}
			idBytes, err := msp.NewSerializedIdentity(s.Identity.MspId, cert)
			if err != nil {
				return err
			}

			signedData[i] = &protoutil.SignedData{
				Data:      data,
				Identity:  idBytes,
				Signature: s.Endorsement,
			}
		}
		return v.verifier.EvaluateSignedData(signedData)
	default:
		return errors.Newf("policy rule [%v] not supported", v.NamespacePolicy.GetRule())
	}
}

// CreateSerializedIdentities creates serialized identities using the mspIDs and certBytes.
func CreateSerializedIdentities(mspIDs, certBytes []string) [][]byte {
	identities := make([][]byte, len(mspIDs))
	for i, mspID := range mspIDs {
		identities[i] = protoutil.MarshalOrPanic(&fmsp.SerializedIdentity{Mspid: mspID, IdBytes: []byte(certBytes[i])})
	}
	return identities
}

// verifier verifies a digest.
type verifier interface {
	verify(Digest, Signature) error
}

func verify(signatureSet []*protoutil.SignedData, v verifier) error {
	for _, s := range signatureSet {
		if err := v.verify(digest(s.Data), s.Signature); err != nil {
			return err
		}
	}
	return nil
}
