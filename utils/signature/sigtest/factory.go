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

	"github.com/consensys/gnark-crypto/ecc/bn254"
	"github.com/consensys/gnark-crypto/ecc/bn254/fr"
	"golang.org/x/crypto/sha3"

	"github.com/hyperledger/fabric-x-committer/utils"
	"github.com/hyperledger/fabric-x-committer/utils/signature"
)

// NewKeys generate private and public keys.
// This should only be used for evaluation and testing as it might use non-secure crypto methods.
func NewKeys(scheme signature.Scheme) (signature.PrivateKey, signature.PublicKey) {
	switch scheme {
	case signature.Ecdsa:
		return ecdsaNewKeys()
	case signature.Bls:
		return blsNewKeys()
	case signature.Eddsa:
		return eddsaNewKeys()
	default:
		return []byte{}, []byte{}
	}
}

// NewKeysWithSeed generate deterministic private and public keys.
// This should only be used for evaluation and testing as it might use non-secure crypto methods.
func NewKeysWithSeed(scheme signature.Scheme, seed int64) (signature.PrivateKey, signature.PublicKey) {
	switch scheme {
	case signature.Ecdsa:
		return ecdsaNewKeysWithSeed(seed)
	case signature.Bls:
		return blsNewKeysWithSeed(seed)
	case signature.Eddsa:
		return eddsaNewKeysWithSeed(seed)
	default:
		return []byte{}, []byte{}
	}
}

func eddsaNewKeys() (signature.PrivateKey, signature.PublicKey) {
	return eddsaNewKeysWithRand(rand.Reader)
}

func eddsaNewKeysWithSeed(seed int64) (signature.PrivateKey, signature.PublicKey) {
	return eddsaNewKeysWithRand(pseudorand.New(pseudorand.NewSource(seed)))
}

func eddsaNewKeysWithRand(rnd io.Reader) (signature.PrivateKey, signature.PublicKey) {
	pk, sk, err := ed25519.GenerateKey(rnd)
	utils.Must(err)
	return sk, pk
}

func blsNewKeys() (signature.PrivateKey, signature.PublicKey) {
	return blsNewKeysWithRand(rand.Reader)
}

func blsNewKeysWithSeed(seed int64) (signature.PrivateKey, signature.PublicKey) {
	return blsNewKeysWithRand(pseudorand.New(pseudorand.NewSource(seed)))
}

func blsNewKeysWithRand(rnd io.Reader) (signature.PrivateKey, signature.PublicKey) {
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

func ecdsaNewKeys() (signature.PrivateKey, signature.PublicKey) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	utils.Must(err)
	serializedPrivateKey, err := SerializeSigningKey(privateKey)
	utils.Must(err)
	serializedPublicKey, err := SerializeVerificationKey(&privateKey.PublicKey)
	utils.Must(err)
	return serializedPrivateKey, serializedPublicKey
}

func ecdsaNewKeysWithSeed(seed int64) (signature.PrivateKey, signature.PublicKey) {
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
