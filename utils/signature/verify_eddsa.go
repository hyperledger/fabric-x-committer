/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package signature

import (
	"crypto/ed25519"
	"errors"

	mspi "github.com/hyperledger/fabric-x-common/msp"
	"github.com/hyperledger/fabric-x-common/protoutil"
)

// edDSAVerifier verifies using the EdDSA scheme.
type edDSAVerifier struct {
	PublicKey ed25519.PublicKey
}

var defaultOptions = ed25519.Options{
	Context: "Example_ed25519ctx",
}

// verify a digest given a signature.
func (v *edDSAVerifier) verify(digest Digest, signature Signature) error {
	err := ed25519.VerifyWithOptions(v.PublicKey, digest, signature, &defaultOptions)
	if err != nil {
		return errors.Join(ErrSignatureMismatch, err)
	}
	return nil
}

// EvaluateSignedData takes a set of SignedData and evaluates whether
// the signatures are valid over the related message.
func (v *edDSAVerifier) EvaluateSignedData(signatureSet []*protoutil.SignedData) error {
	return verify(signatureSet, v)
}

// EvaluateIdentities returns nil as it is not applicable for EcdsaTxVerifier.
func (*edDSAVerifier) EvaluateIdentities(_ []mspi.Identity) error {
	return nil
}
