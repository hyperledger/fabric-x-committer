/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package signature

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"

	"github.com/cockroachdb/errors"
	mspi "github.com/hyperledger/fabric-x-common/msp"
	"github.com/hyperledger/fabric-x-common/protoutil"
)

// ecdsaTxVerifier verifies using the ECDSA scheme.
type ecdsaTxVerifier struct {
	verificationKey *ecdsa.PublicKey
}

// newEcdsaVerifier instantiate a new ECDSA scheme verifier.
func newEcdsaVerifier(key []byte) (*ecdsaTxVerifier, error) {
	block, _ := pem.Decode(key)
	if block == nil || block.Type != "PUBLIC KEY" {
		return nil, errors.Newf("failed to decode PEM block containing public key, got %v", block)
	}

	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, errors.Wrap(err, "cannot parse public key")
	}

	ecdsaVerificationKey, ok := pub.(*ecdsa.PublicKey)
	if !ok {
		return nil, errors.New("failed to assert public key type to ECDSA")
	}
	return &ecdsaTxVerifier{verificationKey: ecdsaVerificationKey}, nil
}

// verify a digest given a signature.
func (v *ecdsaTxVerifier) verify(digest Digest, signature Signature) error {
	valid := ecdsa.VerifyASN1(v.verificationKey, digest, signature)
	if !valid {
		return ErrSignatureMismatch
	}
	return nil
}

// EvaluateSignedData takes a set of SignedData and evaluates whether
// the signatures are valid over the related message.
func (v *ecdsaTxVerifier) EvaluateSignedData(signatureSet []*protoutil.SignedData) error {
	return verify(signatureSet, v)
}

// EvaluateIdentities returns nil as it is not applicable for EcdsaTxVerifier.
func (*ecdsaTxVerifier) EvaluateIdentities(_ []mspi.Identity) error {
	return nil
}
