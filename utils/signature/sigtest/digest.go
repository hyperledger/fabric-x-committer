/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package sigtest

import (
	"crypto/sha256"
	"crypto/sha3"
	"crypto/sha512"
	"crypto/x509"
	"hash"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-lib-go/bccsp"
)

// Digest creates a hash of the content of the passed file.
func Digest(pemCertPath, hashFunc string) ([]byte, error) {
	cert, err := GetCert(pemCertPath)
	if err != nil {
		return nil, err
	}
	return DigestCert(cert, hashFunc)
}

// DigestPemContent creates a hash of the content of the PEM.
func DigestPemContent(pemContent []byte, hashFunc string) ([]byte, error) {
	cert, err := GetCertFromBytes(pemContent)
	if err != nil {
		return nil, err
	}
	return DigestCert(cert, hashFunc)
}

// DigestCert creates a hash of certificate.
func DigestCert(cert *x509.Certificate, hashFunc string) ([]byte, error) {
	var hasher hash.Hash
	switch hashFunc {
	case bccsp.SHA256:
		hasher = sha256.New()
	case bccsp.SHA384:
		hasher = sha512.New384()
	case bccsp.SHA3_256:
		hasher = sha3.New256()
	case bccsp.SHA3_384:
		hasher = sha3.New384()
	default:
		return nil, errors.Newf("unsupported hash function: %s", hashFunc)
	}

	if _, err := hasher.Write(cert.Raw); err != nil {
		return nil, err
	}
	return hasher.Sum(nil), nil
}
