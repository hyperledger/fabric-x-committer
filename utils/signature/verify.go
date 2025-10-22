/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package signature

import (
	"strings"

	"github.com/cockroachdb/errors"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
)

// DigestVerifier verifies a digest.
type DigestVerifier interface {
	Verify(Digest, Signature) error
}

// NsVerifier verifies a given namespace.
type NsVerifier struct {
	verifier  DigestVerifier
	Scheme    Scheme
	PublicKey PublicKey
}

// NewNsVerifier creates a new namespace verifier according to the implementation scheme.
func NewNsVerifier(scheme Scheme, key PublicKey) (*NsVerifier, error) {
	res := &NsVerifier{
		Scheme:    strings.ToUpper(scheme),
		PublicKey: key,
	}
	var err error
	switch res.Scheme {
	case NoScheme, "":
		res.verifier = nil
	case Ecdsa:
		res.verifier, err = NewEcdsaVerifier(key)
	case Bls:
		res.verifier, err = NewBLSVerifier(key)
	case Eddsa:
		res.verifier = &EdDSAVerifier{PublicKey: key}
	default:
		return nil, errors.Newf("scheme '%v' not supported", scheme)
	}
	return res, err
}

// VerifyNs verifies a transaction's namespace signature.
func (v *NsVerifier) VerifyNs(txID string, tx *protoblocktx.Tx, nsIndex int) error {
	if nsIndex < 0 || nsIndex >= len(tx.Namespaces) || nsIndex >= len(tx.Signatures) {
		return errors.New("namespace index out of range")
	}
	if v.verifier == nil {
		return nil
	}
	digest, err := DigestTxNamespace(txID, tx.Namespaces[nsIndex])
	if err != nil {
		return err
	}
	return v.verifier.Verify(digest, tx.Signatures[nsIndex])
}
