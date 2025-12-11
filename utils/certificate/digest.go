/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package certificate

import (
	"crypto/sha256"
	"crypto/sha3"
	"crypto/sha512"
	"hash"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-lib-go/bccsp"

	"github.com/hyperledger/fabric-x-committer/utils/signature/sigtest"
)

// Digest creates a hash of the content of the passed file.
func Digest(pemCertPath, hashFunc string) ([]byte, error) {
	cert, err := sigtest.GetCert(pemCertPath)
	if err != nil {
		return nil, err
	}

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
