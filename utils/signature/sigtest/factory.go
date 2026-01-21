/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package sigtest

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/binary"
	"io"
	"math/big"
	pseudorand "math/rand"
	"strings"

	"github.com/consensys/gnark-crypto/ecc/bn254"
	"github.com/consensys/gnark-crypto/ecc/bn254/fr"
	"golang.org/x/crypto/sha3"

	"github.com/hyperledger/fabric-x-committer/utils"
	"github.com/hyperledger/fabric-x-committer/utils/signature"
)

// NewKeyPair generate private and public keys.
// This should only be used for evaluation and testing as it might use non-secure crypto methods.
func NewKeyPair(scheme signature.Scheme) (signature.PrivateKey, signature.PublicKey) {
	switch strings.ToUpper(scheme) {
	case signature.Ecdsa:
		return ecdsaNewKeyPair()
	case signature.Bls:
		return blsNewKeyPair()
	case signature.Eddsa:
		return eddsaNewKeyPair()
	default:
		return []byte{}, []byte{}
	}
}

// NewKeyPairWithSeed generate deterministic private and public keys.
// This should only be used for evaluation and testing as it might use non-secure crypto methods.
func NewKeyPairWithSeed(scheme signature.Scheme, seed int64) (signature.PrivateKey, signature.PublicKey) {
	switch strings.ToUpper(scheme) {
	case signature.Ecdsa:
		return ecdsaNewKeyPairWithSeed(seed)
	case signature.Bls:
		return blsNewKeyPairWithSeed(seed)
	case signature.Eddsa:
		return eddsaNewKeyPairWithSeed(seed)
	default:
		return []byte{}, []byte{}
	}
}

func eddsaNewKeyPair() (signature.PrivateKey, signature.PublicKey) {
	return eddsaNewKeyPairWithRand(rand.Reader)
}

func eddsaNewKeyPairWithSeed(seed int64) (signature.PrivateKey, signature.PublicKey) {
	// Derive a deterministic, cryptographically strong byte stream from the seed
	// using SHAKE256, which implements io.Reader. This preserves determinism for
	// testing while avoiding use of math/rand as a randomness source.
	var seedBytes [8]byte
	binary.BigEndian.PutUint64(seedBytes[:], uint64(seed)) //nolint:gosec // int64 -> uint64
	sh := sha3.NewShake256()
	_, err := sh.Write(seedBytes[:])
	utils.Must(err)
	return eddsaNewKeyPairWithRand(sh)
}

func eddsaNewKeyPairWithRand(rnd io.Reader) (signature.PrivateKey, signature.PublicKey) {
	pk, sk, err := ed25519.GenerateKey(rnd)
	utils.Must(err)
	return sk, pk
}

func blsNewKeyPair() (signature.PrivateKey, signature.PublicKey) {
	return blsNewKeyPairWithRand(rand.Reader)
}

func blsNewKeyPairWithSeed(seed int64) (signature.PrivateKey, signature.PublicKey) {
	return blsNewKeyPairWithRand(pseudorand.New(pseudorand.NewSource(seed)))
}

func blsNewKeyPairWithRand(rnd io.Reader) (signature.PrivateKey, signature.PublicKey) {
	_, _, _, g2 := bn254.Generators()

	randomBytes := utils.MustRead(rnd, fr.Modulus().BitLen())

	sk := big.NewInt(0)
	sk.SetBytes(randomBytes)
	sk.Mod(sk, fr.Modulus())

	var pk bn254.G2Affine
	pk.ScalarMultiplication(&g2, sk)
	pkBytes := pk.Bytes()

	return sk.Bytes(), pkBytes[:]
}

func ecdsaNewKeyPair() (signature.PrivateKey, signature.PublicKey) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	utils.Must(err)
	serializedPrivateKey, err := SerializeSigningKey(privateKey)
	utils.Must(err)
	serializedPublicKey, err := SerializeVerificationKey(&privateKey.PublicKey)
	utils.Must(err)
	return serializedPrivateKey, serializedPublicKey
}

func ecdsaNewKeyPairWithSeed(seed int64) (signature.PrivateKey, signature.PublicKey) {
	curve := elliptic.P256()

	seedBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(seedBytes, uint64(seed)) //nolint:gosec // integer overflow conversion int64 -> uint64

	hash := sha3.New256()
	hash.Write(seedBytes)
	privateKey := new(big.Int).SetBytes(hash.Sum(nil))

	privateKey.Mod(privateKey, curve.Params().N)
	if privateKey.Sign() == 0 {
		// generated zero private key
		return nil, nil
	}

	x, y := curve.ScalarBaseMult(privateKey.Bytes())
	pk := &ecdsa.PrivateKey{
		PublicKey: ecdsa.PublicKey{Curve: curve, X: x, Y: y},
		D:         privateKey,
	}
	serializedPrivateKey, err := SerializeSigningKey(pk)
	utils.Must(err)
	serializedPublicKey, err := SerializeVerificationKey(&pk.PublicKey)
	utils.Must(err)
	return serializedPrivateKey, serializedPublicKey
}
