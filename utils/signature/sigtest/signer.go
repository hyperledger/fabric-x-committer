/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package sigtest

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"math/big"
	"strings"

	"github.com/cockroachdb/errors"
	"github.com/consensys/gnark-crypto/ecc/bn254"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
	"github.com/hyperledger/fabric-x-committer/utils/signature"
)

// Signer types.
type (
	// NsSigner signs a transaction's namespace.
	// It also implements DigestSigner.
	NsSigner struct {
		signer DigestSigner
	}

	// DigestSigner is an interface for signing a digest.
	DigestSigner interface {
		Sign(signature.Digest) (signature.Signature, error)
	}

	// EddsaSigner signs using the EDDSA scheme.
	EddsaSigner struct {
		PrivateKey ed25519.PrivateKey
	}

	// BlsSigner signs using the BLS scheme.
	BlsSigner struct {
		sk *big.Int
	}

	// EcdsaSigner signs using the ECDSA scheme.
	EcdsaSigner struct {
		signingKey *ecdsa.PrivateKey
	}
)

var dummySig = make([]byte, 0)

// NewNsSigner creates a new namespace signer according to the implementation scheme.
func NewNsSigner(scheme signature.Scheme, key []byte) (*NsSigner, error) {
	scheme = strings.ToUpper(scheme)
	var err error
	var s DigestSigner
	switch scheme {
	case signature.NoScheme, "":
		s = nil
	case signature.Ecdsa:
		s, err = NewEcdsaSigner(key)
	case signature.Bls:
		s = NewBlsSigner(key)
	case signature.Eddsa:
		s = &EddsaSigner{PrivateKey: key}
	default:
		return nil, errors.Newf("scheme '%v' not supported", scheme)
	}
	return &NsSigner{signer: s}, errors.Wrap(err, "failed creating signer")
}

// NewBlsSigner instantiate a BlsSigner given a key.
func NewBlsSigner(key signature.PrivateKey) *BlsSigner {
	sk := big.NewInt(0)
	sk.SetBytes(key)
	return &BlsSigner{sk}
}

// NewEcdsaSigner instantiate a EcdsaSigner given a key.
func NewEcdsaSigner(key signature.PrivateKey) (*EcdsaSigner, error) {
	signingKey, err := ParseSigningKey(key)
	if err != nil {
		return nil, err
	}
	return &EcdsaSigner{signingKey: signingKey}, nil
}

// SignNs signs a transaction's namespace.
func (v *NsSigner) SignNs(tx *protoblocktx.Tx, nsIndex int) (signature.Signature, error) {
	if nsIndex < 0 || nsIndex >= len(tx.Namespaces) {
		return nil, errors.New("namespace index out of range")
	}
	if v.signer == nil {
		return dummySig, nil
	}
	digest, err := signature.DigestTxNamespace(tx, nsIndex)
	if err != nil {
		return nil, errors.Wrap(err, "failed creating digest")
	}
	return v.signer.Sign(digest)
}

// Sign signs a digest.
func (b *EddsaSigner) Sign(digest signature.Digest) (signature.Signature, error) {
	sig, err := b.PrivateKey.Sign(nil, digest, &ed25519.Options{
		Context: "Example_ed25519ctx",
	})
	return sig, errors.Wrap(err, "signing failed")
}

// Sign signs a digest.
func (b *BlsSigner) Sign(digest signature.Digest) (signature.Signature, error) {
	g1h, err := bn254.HashToG1(digest, []byte(signature.BlsHashPrefix))
	if err != nil {
		return nil, errors.Wrap(err, "signing failed")
	}

	sig := g1h.ScalarMultiplication(&g1h, b.sk).Bytes()
	return sig[:], nil
}

// Sign signs a digest.
func (s *EcdsaSigner) Sign(digest signature.Digest) (signature.Signature, error) {
	sig, err := ecdsa.SignASN1(rand.Reader, s.signingKey, digest)
	return sig, errors.Wrap(err, "failed to sign transaction")
}
