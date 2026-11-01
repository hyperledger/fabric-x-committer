/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package testsig

import (
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"math/big"

	"github.com/cockroachdb/errors"
	"github.com/consensys/gnark-crypto/ecc/bn254"
	"golang.org/x/crypto/cryptobyte"
	cryptobyteasn1 "golang.org/x/crypto/cryptobyte/asn1"

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

	// ecdsaSigner signs using the ECDSA scheme with a plain random nonce.
	//
	// crypto/ecdsa.SignASN1 builds a fresh HMAC-SHA-512 DRBG per signature to derive its
	// nonce. That DRBG cost the load generator more CPU than the k*G it feeds, plus most of
	// its allocations; BenchmarkEcdsaSigners measures it. It guards a real private key against
	// a broken RNG, which buys nothing for the throwaway keys this package signs with.
	// The result is ordinary ECDSA that crypto/ecdsa.VerifyASN1 accepts.
	ecdsaSigner struct {
		// The signing key's curve, as crypto/ecdh sees it. Its GenerateKey is how we get a
		// nonce: see Sign.
		curve ecdh.Curve
		order *big.Int
		// The secret scalar, read once: PrivateKey.D is deprecated, PrivateKey.Bytes is not.
		secret *big.Int
		// Upper bound on the DER signature size, so its builder never has to grow.
		derSize int
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

// newEcdsaSigner builds a signer over the given key's curve order.
// It fails on curves crypto/ecdh does not support, which is P-224 and nothing else.
func newEcdsaSigner(signingKey *ecdsa.PrivateKey) (*ecdsaSigner, error) {
	// Only for its curve; this key is never used for key exchange.
	ecdhKey, err := signingKey.ECDH()
	if err != nil {
		return nil, errors.Wrap(err, "unsupported ECDSA curve")
	}
	secret, err := signingKey.Bytes()
	if err != nil {
		return nil, errors.Wrap(err, "failed to read ECDSA signing key")
	}
	order := signingKey.Params().N
	// Each of r and s takes at most a 2-byte header, a leading zero and the coordinate; the
	// SEQUENCE around them takes at most 3. Coming out too large only wastes a few bytes.
	coordinate := (order.BitLen() + 7) / 8
	return &ecdsaSigner{
		curve:   ecdhKey.Curve(),
		order:   order,
		secret:  new(big.Int).SetBytes(secret),
		derSize: 2*(coordinate+3) + 3,
	}, nil
}

// Sign signs a digest.
func (s *ecdsaSigner) Sign(digest signature.Digest) (signature.Signature, error) {
	// A key pair is exactly what a nonce is: GenerateKey draws a uniform scalar k in
	// [1, n-1] and returns k*G with it, which is all ECDSA needs from the curve.
	nonce, err := s.curve.GenerateKey(rand.Reader)
	if err != nil {
		return nil, errors.Wrap(err, "failed to generate nonce")
	}
	k := new(big.Int).SetBytes(nonce.Bytes())

	// r is the x-coordinate of k*G, mod n. Bytes() is 0x04 || x || y, both coordinates the
	// same width.
	point := nonce.PublicKey().Bytes()
	r := new(big.Int).SetBytes(point[1 : 1+(len(point)-1)/2])
	r.Mod(r, s.order)
	if r.Sign() == 0 {
		return s.Sign(digest)
	}

	// sig = k^-1 * (e + r*d) mod n.
	sig := new(big.Int).Mul(r, s.secret)
	sig.Add(sig, hashToInt(digest, s.order))
	sig.Mul(sig, new(big.Int).ModInverse(k, s.order))
	sig.Mod(sig, s.order)
	if sig.Sign() == 0 {
		return s.Sign(digest)
	}

	return s.marshalSignatureDER(r, sig)
}

// hashToInt converts a digest to an integer as SEC 1 section 4.1.3 step 5 specifies.
// Both adjustments are no-ops for P-256 with SHA-256, but keys may use a larger curve.
func hashToInt(digest signature.Digest, order *big.Int) *big.Int {
	orderBytes := (order.BitLen() + 7) / 8
	if len(digest) > orderBytes {
		digest = digest[:orderBytes]
	}
	e := new(big.Int).SetBytes(digest)
	if excess := len(digest)*8 - order.BitLen(); excess > 0 {
		e.Rsh(e, uint(excess))
	}
	return e
}

// marshalSignatureDER encodes (r, s) as the DER SEQUENCE crypto/ecdsa.VerifyASN1 expects.
// cryptobyte, not encoding/asn1: no reflection, and the same bytes SignASN1 would produce.
func (s *ecdsaSigner) marshalSignatureDER(r, sig *big.Int) ([]byte, error) {
	b := cryptobyte.NewBuilder(make([]byte, 0, s.derSize))
	b.AddASN1(cryptobyteasn1.SEQUENCE, func(seq *cryptobyte.Builder) {
		seq.AddASN1BigInt(r)
		seq.AddASN1BigInt(sig)
	})
	der, err := b.Bytes()
	return der, errors.Wrap(err, "failed to encode signature")
}
