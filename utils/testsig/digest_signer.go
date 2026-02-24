/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package testsig

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"math/big"

	"github.com/cockroachdb/errors"
	"github.com/consensys/gnark-crypto/ecc/bn254"

	"github.com/hyperledger/fabric-x-committer/utils/signature"
)

type (
	// eddsaSigner signs using the EDDSA scheme.
	eddsaSigner struct {
		PrivateKey ed25519.PrivateKey
	}

	// blsSigner signs using the BLS scheme.
	blsSigner struct {
		sk *big.Int
	}

	// ecdsaSigner signs using the ECDSA scheme.
	ecdsaSigner struct {
		signingKey *ecdsa.PrivateKey
	}
)

// Sign signs a digest.
func (b *eddsaSigner) Sign(digest signature.Digest) (signature.Signature, error) {
	sig, err := b.PrivateKey.Sign(nil, digest, &ed25519.Options{
		Context: "Example_ed25519ctx",
	})
	return sig, errors.Wrap(err, "signing failed")
}

// Sign signs a digest.
func (b *blsSigner) Sign(digest signature.Digest) (signature.Signature, error) {
	g1h, err := bn254.HashToG1(digest, []byte(signature.BlsHashPrefix))
	if err != nil {
		return nil, errors.Wrap(err, "signing failed")
	}
	sig := g1h.ScalarMultiplication(&g1h, b.sk).Bytes()
	return sig[:], nil
}

// Sign signs a digest.
func (s *ecdsaSigner) Sign(digest signature.Digest) (signature.Signature, error) {
	sig, err := ecdsa.SignASN1(rand.Reader, s.signingKey, digest)
	return sig, errors.Wrap(err, "failed to sign transaction")
}
