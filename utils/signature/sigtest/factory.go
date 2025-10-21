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
	"math/big"
	pseudorand "math/rand"
	"strings"

	"github.com/cockroachdb/errors"
	"github.com/consensys/gnark-crypto/ecc/bn254"
	"github.com/consensys/gnark-crypto/ecc/bn254/fr"
	"golang.org/x/crypto/sha3"

	"github.com/hyperledger/fabric-x-committer/utils"
	"github.com/hyperledger/fabric-x-committer/utils/signature"
)

type (
	// SignerVerifierFactory implements KeyFactory and instantiate verifiers and signers.
	// This should only be used for evaluation and testing as it might use non-secure
	// crypto methods.
	SignerVerifierFactory struct {
		KeyFactory
		scheme signature.Scheme
	}

	// KeyFactory generates public and private keys.
	KeyFactory interface {
		NewKeys() (signature.PrivateKey, signature.PublicKey)
		NewKeysWithSeed(int64) (signature.PrivateKey, signature.PublicKey)
	}

	// KeyFactory implementations.
	dummyFactory struct{}
	eddsaFactory struct{}
	blsFactory   struct{}
	ecdsaFactory struct{}
)

// String returns a string representation for logging.
func (f *SignerVerifierFactory) String() string {
	return f.Scheme()
}

// Scheme returns the factory scheme.
func (f *SignerVerifierFactory) Scheme() signature.Scheme {
	return f.scheme
}

// NewVerifier instantiate a verifier given a public key.
func (f *SignerVerifierFactory) NewVerifier(key signature.PublicKey) (*signature.NsVerifier, error) {
	return signature.NewNsVerifier(f.scheme, key)
}

// NewSigner instantiate a signer given a private key.
func (f *SignerVerifierFactory) NewSigner(key signature.PrivateKey) (*NsSigner, error) {
	return NewNsSigner(f.scheme, key)
}

// NewSignatureFactory instantiate a SignerVerifierFactory.
func NewSignatureFactory(scheme signature.Scheme) *SignerVerifierFactory {
	f, err := getFactory(scheme)
	if err != nil {
		panic(err.Error())
	}
	return f
}

func getFactory(scheme signature.Scheme) (*SignerVerifierFactory, error) {
	scheme = strings.ToUpper(scheme)
	factory := &SignerVerifierFactory{scheme: scheme}
	switch scheme {
	case signature.NoScheme, "":
		factory.KeyFactory = &dummyFactory{}
	case signature.Ecdsa:
		factory.KeyFactory = &ecdsaFactory{}
	case signature.Bls:
		factory.KeyFactory = &blsFactory{}
	case signature.Eddsa:
		factory.KeyFactory = &eddsaFactory{}
	default:
		return nil, errors.Newf("scheme '%v' not supported", scheme)
	}
	return factory, nil
}

// dummy

// NewKeys generate private and public keys.
func (f *dummyFactory) NewKeys() (signature.PrivateKey, signature.PublicKey) {
	return f.NewKeysWithSeed(0)
}

// NewKeysWithSeed generate deterministic private and public keys.
func (*dummyFactory) NewKeysWithSeed(int64) (signature.PrivateKey, signature.PublicKey) {
	return []byte{}, []byte{}
}

// eddsa

// NewKeys generate private and public keys.
func (b *eddsaFactory) NewKeys() (signature.PrivateKey, signature.PublicKey) {
	return b.NewKeysWithSeed(pseudorand.Int63())
}

// NewKeysWithSeed generate deterministic private and public keys.
func (*eddsaFactory) NewKeysWithSeed(seed int64) (signature.PrivateKey, signature.PublicKey) {
	r := pseudorand.New(pseudorand.NewSource(seed))
	pk, sk, err := ed25519.GenerateKey(r)
	utils.Must(err)
	return sk, pk
}

// bls

// NewKeys generate private and public keys.
func (b *blsFactory) NewKeys() (signature.PrivateKey, signature.PublicKey) {
	return b.NewKeysWithSeed(pseudorand.Int63())
}

// NewKeysWithSeed generate deterministic private and public keys.
func (*blsFactory) NewKeysWithSeed(seed int64) (signature.PrivateKey, signature.PublicKey) {
	r := pseudorand.New(pseudorand.NewSource(seed))
	_, _, _, g2 := bn254.Generators()

	randomBytes := utils.MustRead(r, fr.Modulus().BitLen())

	sk := big.NewInt(0)
	sk.SetBytes(randomBytes)
	sk.Mod(sk, fr.Modulus())

	var pk bn254.G2Affine
	pk.ScalarMultiplication(&g2, sk)
	pkBytes := pk.Bytes()

	return sk.Bytes(), pkBytes[:]
}

// ecdsa

// NewKeys generate private and public keys.
func (*ecdsaFactory) NewKeys() (signature.PrivateKey, signature.PublicKey) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	utils.Must(err)
	serializedPrivateKey, err := SerializeSigningKey(privateKey)
	utils.Must(err)
	serializedPublicKey, err := SerializeVerificationKey(&privateKey.PublicKey)
	utils.Must(err)
	return serializedPrivateKey, serializedPublicKey
}

// NewKeysWithSeed generate deterministic private and public keys.
func (*ecdsaFactory) NewKeysWithSeed(seed int64) (signature.PrivateKey, signature.PublicKey) {
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
