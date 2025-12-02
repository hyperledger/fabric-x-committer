/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package signature

import (
	"github.com/cockroachdb/errors"
)

type (
	// Digest of a message.
	Digest = []byte
	// Signature of a message.
	Signature = []byte
	// PrivateKey to be used.
	PrivateKey = []byte
	// PublicKey to be used.
	PublicKey = []byte
	// Scheme to be used.
	Scheme = string
	// Policy defining the governance.
	Policy = []byte
)

// Supported schemes.
const (
	// NoScheme does nothing.
	NoScheme Scheme = "NONE"
	// Ecdsa use the ECDSA scheme.
	Ecdsa Scheme = "ECDSA"
	// Bls use the BLS scheme.
	Bls Scheme = "BLS"
	// Eddsa use the EDDSA scheme.
	Eddsa Scheme = "EDDSA"
)

var (
	// ErrSignatureMismatch is returned when a verifier detect a wrong signature.
	ErrSignatureMismatch = errors.New("signature mismatch")
	// AllSchemes all the supported scheme.
	AllSchemes = []Scheme{
		NoScheme, Ecdsa, Bls, Eddsa,
	}
	// AllRealSchemes all supported real scheme (excluding NoScheme).
	AllRealSchemes = []Scheme{
		Ecdsa, Bls, Eddsa,
	}
)
