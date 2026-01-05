/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package signature

import (
	"crypto/ed25519"
	"errors"
)

// edDSAVerifier verifies using the EdDSA scheme.
type edDSAVerifier struct {
	PublicKey ed25519.PublicKey
}

var defaultOptions = ed25519.Options{
	Context: "Example_ed25519ctx",
}

// verifyDigest verifies a digest given a signature.
func (v *edDSAVerifier) verifyDigest(digest Digest, signature Signature) error {
	err := ed25519.VerifyWithOptions(v.PublicKey, digest, signature, &defaultOptions)
	if err != nil {
		return errors.Join(ErrSignatureMismatch, err)
	}
	return nil
}
