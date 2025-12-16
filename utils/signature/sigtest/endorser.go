/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package sigtest

import (
	"math/big"
	"strings"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-lib-go/bccsp"
	"github.com/hyperledger/fabric-x-common/msp"
	"github.com/hyperledger/fabric-x-common/protoutil"

	"github.com/hyperledger/fabric-x-committer/api/applicationpb"
	"github.com/hyperledger/fabric-x-committer/utils/signature"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

// TxNsEndorser endorse a transaction's namespace.
// It converts a TX into a ASN1 message, and then uses the message endorser interface to endorse it.
// It also implements the message-endorser interface.
type TxNsEndorser struct {
	messageEndorser
}

var dummyEndorsement = CreateEndorsementsForThresholdRule(make([]byte, 0))[0]

// NewRawKeyEndorser creates a new namespace endorser according to the implementation scheme.
func NewRawKeyEndorser(scheme signature.Scheme, key []byte) (*TxNsEndorser, error) {
	scheme = strings.ToUpper(scheme)
	var err error
	ret := &TxNsEndorser{}
	switch scheme {
	case signature.NoScheme, "":
		ret.messageEndorser = nil
	case signature.Ecdsa:
		signingKey, parseErr := ParseSigningKey(key)
		err = parseErr
		ret.messageEndorser = &rawKeyMessageEndorser{signer: &ecdsaSigner{signingKey: signingKey}}
	case signature.Bls:
		sk := big.NewInt(0)
		sk.SetBytes(key)
		ret.messageEndorser = &rawKeyMessageEndorser{signer: &blsSigner{sk}}
	case signature.Eddsa:
		ret.messageEndorser = &rawKeyMessageEndorser{signer: &eddsaSigner{PrivateKey: key}}
	default:
		return nil, errors.Newf("scheme '%v' not supported", scheme)
	}
	return ret, err
}

// NewMspEndorser creates a new mspMessageEndorser given MSP directories.
// This signer will create an endorsement for each MSP provided.
func NewMspEndorser(certType int, mspDirs ...msp.DirLoadParameters) (*TxNsEndorser, error) {
	identities, err := GetSigningIdentities(mspDirs...)
	if err != nil {
		return nil, err
	}
	mspEndorser := &mspMessageEndorser{
		certType:   certType,
		identities: identities,
		mspIDs:     make([][]byte, len(mspDirs)),
		certsBytes: make([][]byte, len(mspDirs)),
	}
	for i, id := range identities {
		mspEndorser.mspIDs[i] = []byte(id.GetMSPIdentifier())
		serializedIDBytes, err := id.Serialize()
		if err != nil {
			return nil, errors.Wrap(err, "serializing default signing identity")
		}
		serializedID, err := protoutil.UnmarshalSerializedIdentity(serializedIDBytes)
		if err != nil {
			return nil, err
		}
		idBytes := serializedID.IdBytes
		if certType == test.CreatorID {
			idBytes, err = DigestPemContent(idBytes, bccsp.SHA256)
			if err != nil {
				return nil, err
			}
		}
		mspEndorser.certsBytes[i] = idBytes
	}
	return &TxNsEndorser{messageEndorser: mspEndorser}, nil
}

// EndorseTxNs endorses a transaction's namespace.
func (v *TxNsEndorser) EndorseTxNs(txID string, tx *applicationpb.Tx, nsIdx int) (*applicationpb.Endorsements, error) {
	if nsIdx < 0 || nsIdx >= len(tx.Namespaces) {
		return nil, errors.New("namespace index out of range")
	}
	if v.messageEndorser == nil {
		return dummyEndorsement, nil
	}
	msg, err := signature.ASN1MarshalTxNamespace(txID, tx.Namespaces[nsIdx])
	if err != nil {
		return nil, err
	}
	return v.EndorseMessage(msg)
}

// GetSigningIdentities loads signing identities from the given MSP directories.
func GetSigningIdentities(mspDirs ...msp.DirLoadParameters) ([]msp.SigningIdentity, error) {
	identities := make([]msp.SigningIdentity, len(mspDirs))
	for i, mspDir := range mspDirs {
		localMsp, err := msp.LoadLocalMspDir(mspDir)
		if err != nil {
			return nil, err
		}
		identities[i], err = localMsp.GetDefaultSigningIdentity()
		if err != nil {
			return nil, errors.Wrap(err, "loading signing identity")
		}
	}
	return identities, nil
}
