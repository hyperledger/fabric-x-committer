/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package testsig

import (
	"math/big"
	"strings"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-lib-go/bccsp"
	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/hyperledger/fabric-x-common/msp"
	"github.com/hyperledger/fabric-x-common/utils/certificate"

	"github.com/hyperledger/fabric-x-committer/utils/signature"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

// NsEndorser endorse a transaction's namespace.
// It converts a TX into a ASN1 message, and then uses the message endorser interface to endorse it.
// It also implements the endorser interface, which can endorse raw messages.
type NsEndorser struct {
	endorser
}

var dummyEndorsement = CreateEndorsementsForThresholdRule(make([]byte, 0))[0]

// NewNsEndorserFromKey creates a new NsEndorser according to the key and scheme.
func NewNsEndorserFromKey(scheme signature.Scheme, key []byte) (*NsEndorser, error) {
	var err error
	var e endorser
	switch strings.ToUpper(scheme) {
	case signature.NoScheme, "":
		e = nil
	case signature.Ecdsa:
		signingKey, parseErr := ParseSigningKey(key)
		err = parseErr
		e = &keyEndorser{signer: &ecdsaSigner{signingKey: signingKey}}
	case signature.Bls:
		sk := big.NewInt(0)
		sk.SetBytes(key)
		e = &keyEndorser{signer: &blsSigner{sk}}
	case signature.Eddsa:
		e = &keyEndorser{signer: &eddsaSigner{PrivateKey: key}}
	default:
		return nil, errors.Newf("scheme '%v' not supported", scheme)
	}
	return &NsEndorser{endorser: e}, err
}

// NewNsEndorserFromMsp creates a new NsEndorser using identities.
// This endorser will create an endorsement for each MSP provided.
func NewNsEndorserFromMsp(certType int, identities ...msp.SigningIdentity) (*NsEndorser, error) {
	e := &mspEndorser{
		certType:   certType,
		identities: identities,
		mspIDs:     make([][]byte, len(identities)),
		certsBytes: make([][]byte, len(identities)),
	}
	for i, id := range identities {
		e.mspIDs[i] = []byte(id.GetMSPIdentifier())
		idBytes, err := id.GetCertificatePEM()
		if err != nil {
			return nil, err
		}
		if certType == test.CreatorID {
			idBytes, err = certificate.DigestPemContent(idBytes, bccsp.SHA256)
			if err != nil {
				return nil, err
			}
		}
		e.certsBytes[i] = idBytes
	}
	return &NsEndorser{endorser: e}, nil
}

// EndorseTxNs endorses a transaction's namespace.
func (v *NsEndorser) EndorseTxNs(txID string, tx *applicationpb.Tx, nsIdx int) (*applicationpb.Endorsements, error) {
	if nsIdx < 0 || nsIdx >= len(tx.Namespaces) {
		return nil, errors.New("namespace index out of range")
	}
	if v.endorser == nil {
		return dummyEndorsement, nil
	}
	msg, err := tx.Namespaces[nsIdx].ASN1Marshal(txID)
	if err != nil {
		return nil, err
	}
	return v.Endorse(msg)
}
