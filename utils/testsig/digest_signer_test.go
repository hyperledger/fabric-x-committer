/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package testsig

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"math/big"
	"testing"

	"github.com/consensys/gnark-crypto/ecc/bn254"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/utils/signature"
)

type digestSigner interface {
	Sign(digest signature.Digest) (signature.Signature, error)
}

func TestDigestSigners(t *testing.T) {
	t.Parallel()
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	curEddsaSigner := &eddsaSigner{PrivateKey: privateKey}

	sk := big.NewInt(12345)
	curBlsSigner := &blsSigner{sk: sk}

	priv, _ := NewKeyPair(signature.Ecdsa)
	signingKey, err := ParseSigningKey(priv)
	require.NoError(t, err)
	curEcdsaSigner, err := newEcdsaSigner(signingKey)
	require.NoError(t, err)

	for _, tc := range []struct {
		name   string
		signer digestSigner
		expLen int
	}{
		{name: signature.Eddsa, signer: curEddsaSigner, expLen: 64},
		{name: signature.Bls, signer: curBlsSigner, expLen: bn254.SizeOfG1AffineCompressed},
		{name: signature.Ecdsa, signer: curEcdsaSigner, expLen: 0}, // Variable length
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			t.Run("successful signing", func(t *testing.T) {
				t.Parallel()
				digest := sha256.Sum256([]byte("test message"))
				sig, err := tc.signer.Sign(digest[:])
				require.NoError(t, err)
				require.NotNil(t, sig)
				require.NotEmpty(t, sig)

				if tc.expLen > 0 {
					require.Len(t, sig, tc.expLen)
				}
			})
			t.Run("different messages produce different signatures", func(t *testing.T) {
				t.Parallel()
				digest1 := sha256.Sum256([]byte("message 1"))
				sig1, err := tc.signer.Sign(digest1[:])
				require.NoError(t, err)

				digest2 := sha256.Sum256([]byte("message 2"))
				sig2, err := tc.signer.Sign(digest2[:])
				require.NoError(t, err)

				require.NotEqual(t, sig1, sig2)
			})
		})
	}
}

// stdlibHedgedSigner is the crypto/ecdsa baseline the shipped signer replaced. It lives here
// rather than beside the signer so the package holds no unused implementation.
type stdlibHedgedSigner struct {
	signingKey *ecdsa.PrivateKey
}

// Sign signs a digest.
func (s stdlibHedgedSigner) Sign(digest signature.Digest) (signature.Signature, error) {
	return ecdsa.SignASN1(rand.Reader, s.signingKey, digest)
}

// digestPoolSize is how many digests the benchmark prepares. Power of two so the wraparound
// is a mask, and small enough that the pool stays in cache and out of the GC's way.
const digestPoolSize = 1024

// BenchmarkEcdsaSigners compares the two ECDSA nonce strategies. Run with -benchmem and a
// high -cpu: the load generator signs from 128 workers, and the DRBG costs allocations as
// much as CPU, so a single-threaded run understates the difference.
func BenchmarkEcdsaSigners(b *testing.B) {
	privateKey, _ := NewKeyPair(signature.Ecdsa)
	signingKey, err := ParseSigningKey(privateKey)
	require.NoError(b, err)

	plain, err := newEcdsaSigner(signingKey)
	require.NoError(b, err)

	for _, tc := range []struct {
		name   string
		signer digestSigner
	}{
		{name: "stdlib-hedged", signer: stdlibHedgedSigner{signingKey}},
		{name: "plain-nonce", signer: plain},
	} {
		b.Run(tc.name, func(b *testing.B) {
			// Digests hashed up front, so only the signing is measured. A fixed pool rather
			// than one per iteration: a b.N-sized array inflates the live heap, which delays
			// GC and makes ns/op fall as b.N rises - hiding the very allocation cost this
			// benchmark exists to compare.
			digests := make([][sha256.Size]byte, digestPoolSize)
			for i := range digests {
				digests[i] = sha256.Sum256([]byte{
					byte(i), byte(i >> 8), byte(i >> 16), byte(i >> 24),
				})
			}

			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				// Every worker walks the pool, so consecutive signatures never share a
				// digest and the DRBG cannot be amortised.
				i := 0
				for pb.Next() {
					digest := &digests[i]
					i = (i + 1) % digestPoolSize
					if _, err := tc.signer.Sign(digest[:]); err != nil {
						b.Fatal(err)
					}
				}
			})
		})
	}
}

// TestFastEcdsaSignerIsVerifiable checks the only thing that makes a hand-derived nonce
// acceptable: an ordinary verifier accepts the result. crypto/ecdsa.VerifyASN1 is that
// verifier, and it is what utils/signature's ECDSA path calls. A failure here means r, s, or
// the DER encoding is wrong.
func TestFastEcdsaSignerIsVerifiable(t *testing.T) {
	t.Parallel()
	privateKey, _ := NewKeyPair(signature.Ecdsa)
	signingKey, err := ParseSigningKey(privateKey)
	require.NoError(t, err)

	signer, err := newEcdsaSigner(signingKey)
	require.NoError(t, err)

	// Many digests, not one: DER drops leading zero bytes, so a short r or s only shows up
	// once every few hundred signatures.
	for i := range 512 {
		digest := sha256.Sum256([]byte{byte(i), byte(i >> 8)})
		sig, signErr := signer.Sign(digest[:])
		require.NoError(t, signErr)
		require.True(t, ecdsa.VerifyASN1(&signingKey.PublicKey, digest[:], sig),
			"signature %d rejected by ecdsa.VerifyASN1", i)
	}

	// The converse, so the loop above cannot pass by the verifier accepting anything.
	d1 := sha256.Sum256([]byte("signed"))
	d2 := sha256.Sum256([]byte("not signed"))
	sig, err := signer.Sign(d1[:])
	require.NoError(t, err)
	require.False(t, ecdsa.VerifyASN1(&signingKey.PublicKey, d2[:], sig))
}
