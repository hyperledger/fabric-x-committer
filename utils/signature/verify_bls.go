/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package signature

import (
	"github.com/cockroachdb/errors"
	"github.com/consensys/gnark-crypto/ecc/bn254"
	mspi "github.com/hyperledger/fabric-x-common/msp"
	"github.com/hyperledger/fabric-x-common/protoutil"
)

var q []bn254.G2Affine

func init() {
	_, _, _, g2 := bn254.Generators()
	q = []bn254.G2Affine{g2}
}

// BlsHashPrefix is the prefix used to verify a BLS scheme signature.
const BlsHashPrefix = "BLS"

// blsVerifier verifies using the BLS scheme.
type blsVerifier struct {
	pk bn254.G2Affine
}

// newBLSVerifier instantiate a new BLS scheme verifier.
func newBLSVerifier(key []byte) (*blsVerifier, error) {
	var pk bn254.G2Affine
	_, err := pk.SetBytes(key)
	if err != nil {
		return nil, errors.Wrap(err, "cannot set G2 from verification key bytes")
	}
	return &blsVerifier{pk: pk}, nil
}

// verify a digest given a signature.
func (v *blsVerifier) verify(digest Digest, signature Signature) error {
	var sig bn254.G1Affine
	_, err := sig.SetBytes(signature)
	if err != nil {
		return errors.Wrap(err, "cannot set G1 from signature bytes")
	}

	g1h, err := bn254.HashToG1(digest, []byte(BlsHashPrefix))
	if err != nil {
		return errors.Wrap(err, "cannot convert hash to G1")
	}

	left, err := bn254.Pair([]bn254.G1Affine{sig}, q)
	if err != nil {
		return errors.Wrap(err, "cannot pair G2 with signature")
	}

	right, err := bn254.Pair([]bn254.G1Affine{g1h}, []bn254.G2Affine{v.pk})
	if err != nil {
		return errors.Wrap(err, "cannot pair public key with digest")
	}

	if (&left).Equal(&right) {
		return nil
	}
	return ErrSignatureMismatch
}

// EvaluateSignedData takes a set of SignedData and evaluates whether
// the signatures are valid over the related message.
func (v *blsVerifier) EvaluateSignedData(signatureSet []*protoutil.SignedData) error {
	return verifySignedData(signatureSet, v)
}

// EvaluateIdentities returns nil as it is not applicable for EcdsaTxVerifier.
func (*blsVerifier) EvaluateIdentities(_ []mspi.Identity) error {
	return nil
}
