/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package sigtest

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"

	"github.com/cockroachdb/errors"

	"github.com/hyperledger/fabric-x-committer/utils/signature"
)

// SerializeVerificationKey encodes a ECDSA public key into a PEM file.
func SerializeVerificationKey(key *ecdsa.PublicKey) ([]byte, error) {
	if key == nil {
		return nil, errors.New("key is nil")
	}
	x509encodedPub, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		return nil, errors.Wrap(err, "cannot serialize public key")
	}
	return pem.EncodeToMemory(&pem.Block{
		Type:  "PUBLIC KEY",
		Bytes: x509encodedPub,
	}), nil
}

// SerializeSigningKey encodes a ECDSA private key into a PEM file.
func SerializeSigningKey(key *ecdsa.PrivateKey) ([]byte, error) {
	if key == nil {
		return nil, errors.New("key is nil")
	}

	x509encodedPri, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, errors.Wrap(err, "cannot serialize private key")
	}
	return pem.EncodeToMemory(&pem.Block{
		Type:  "EC PRIVATE KEY",
		Bytes: x509encodedPri,
	}), nil
}

// ParseSigningKey decodes a ECDSA key from a PEM file.
func ParseSigningKey(keyContent []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(keyContent)
	if block == nil {
		return nil, errors.New("nil block")
	}
	switch block.Type {
	case "PRIVATE KEY":
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, errors.Wrap(err, "cannot parse private key")
		}
		pk, ok := key.(*ecdsa.PrivateKey)
		if !ok {
			return nil, errors.New("invalid private key")
		}
		return pk, nil
	case "EC PRIVATE KEY":
		key, err := x509.ParseECPrivateKey(block.Bytes)
		return key, errors.Wrap(err, "cannot parse private key")
	default:
		return nil, errors.Newf("unknown block type: %s", block.Type)
	}
}

// GetSerializedKeyFromCert reads a ECDSA public key from a certificate file.
func GetSerializedKeyFromCert(certPath string) (signature.PublicKey, error) {
	cert, err := GetCert(certPath)
	if err != nil {
		return nil, err
	}

	pk, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if cert.PublicKeyAlgorithm != x509.ECDSA || !ok {
		return nil, errors.New("pubkey not ECDSA")
	}

	return SerializeVerificationKey(pk)
}

// GetCert reads a PEM-encoded X.509 certificate from the specified file path.
// and returns the parsed certificate.
func GetCert(certPath string) (*x509.Certificate, error) {
	pemContent, err := os.ReadFile(filepath.Clean(certPath))
	if err != nil {
		return nil, errors.Wrap(err, "cannot read certificate")
	}
	return GetCertFromBytes(pemContent)
}

// GetCertFromBytes reads a PEM-encoded X.509 certificate and returns the parsed certificate.
func GetCertFromBytes(pemContent []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(pemContent)
	if block == nil {
		return nil, errors.New("no PEM content in file")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, errors.Wrap(err, "cannot parse cert")
	}
	return cert, nil
}
