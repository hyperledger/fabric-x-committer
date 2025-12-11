/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package sigtest

import (
	"crypto/sha256"
	"encoding/hex"
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

type (
	// NsEndorser endorse a transaction's namespace.
	// It also implements MessageEndorser.
	NsEndorser struct {
		MessageEndorser
	}

	// MessageEndorser endorse a message.
	MessageEndorser interface {
		EndorseMessage([]byte) (*applicationpb.Endorsements, error)
	}

	// MspEndorser endorse a message using identities loaded from MSP directories.
	MspEndorser struct {
		certType   int
		identities []msp.SigningIdentity
		mspIDs     [][]byte
		certsBytes [][]byte
	}

	// messageEndorserUsingKeySigner endorse a message's digest using a signer.
	messageEndorserUsingKeySigner struct {
		signer interface {
			Sign(signature.Digest) (signature.Signature, error)
		}
	}
)

var dummyEndorsement = CreateEndorsementsForThresholdRule(make([]byte, 0))[0]

// NewNsEndorser creates a new namespace endorser according to the implementation scheme.
func NewNsEndorser(scheme signature.Scheme, key []byte) (*NsEndorser, error) {
	scheme = strings.ToUpper(scheme)
	var err error
	ret := &NsEndorser{}
	switch scheme {
	case signature.NoScheme, "":
		ret.MessageEndorser = nil
	case signature.Ecdsa:
		signingKey, parseErr := ParseSigningKey(key)
		err = parseErr
		ret.MessageEndorser = &messageEndorserUsingKeySigner{signer: &ecdsaSigner{signingKey: signingKey}}
	case signature.Bls:
		sk := big.NewInt(0)
		sk.SetBytes(key)
		ret.MessageEndorser = &messageEndorserUsingKeySigner{signer: &blsSigner{sk}}
	case signature.Eddsa:
		ret.MessageEndorser = &messageEndorserUsingKeySigner{signer: &eddsaSigner{PrivateKey: key}}
	default:
		return nil, errors.Newf("scheme '%v' not supported", scheme)
	}
	return ret, err
}

// NewMspEndorser creates a new MspEndorser given MSP directories.
// This signer will create an endorsement for each MSP provided.
func NewMspEndorser(certType int, mspDirs ...msp.DirLoadParameters) (*NsEndorser, error) {
	identities, err := GetSigningIdentities(mspDirs...)
	if err != nil {
		return nil, err
	}
	mspEndorser := &MspEndorser{
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
	return &NsEndorser{MessageEndorser: mspEndorser}, nil
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

// EndorseTxNs endorses a transaction's namespace.
func (v *NsEndorser) EndorseTxNs(txID string, tx *applicationpb.Tx, nsIndex int) (*applicationpb.Endorsements, error) {
	if nsIndex < 0 || nsIndex >= len(tx.Namespaces) {
		return nil, errors.New("namespace index out of range")
	}
	if v.MessageEndorser == nil {
		return dummyEndorsement, nil
	}
	msg, err := signature.ASN1MarshalTxNamespace(txID, tx.Namespaces[nsIndex])
	if err != nil {
		return nil, err
	}
	return v.EndorseMessage(msg)
}

// EndorseMessage endorses a digest of the given message.
func (d *messageEndorserUsingKeySigner) EndorseMessage(msg []byte) (*applicationpb.Endorsements, error) {
	digest := sha256.Sum256(msg)
	sig, err := d.signer.Sign(digest[:])
	return CreateEndorsementsForThresholdRule(sig)[0], err
}

// EndorseMessage creates endorsements for all identities in the MspEndorser.
func (s *MspEndorser) EndorseMessage(msg []byte) (*applicationpb.Endorsements, error) {
	signatures := make([][]byte, len(s.mspIDs))
	for i, id := range s.identities {
		var err error
		signatures[i], err = id.Sign(msg)
		if err != nil {
			return nil, errors.Wrap(err, "signing failed")
		}
	}
	return CreateEndorsementsForSignatureRule(signatures, s.mspIDs, s.certsBytes, s.certType), nil
}

// CreateEndorsementsForThresholdRule creates a slice of EndorsementSet pointers from individual threshold signatures.
// Each signature provided is wrapped in its own EndorsementWithIdentity and then placed
// in its own new EndorsementSet.
func CreateEndorsementsForThresholdRule(signatures ...[]byte) []*applicationpb.Endorsements {
	sets := make([]*applicationpb.Endorsements, 0, len(signatures))

	for _, sig := range signatures {
		sets = append(sets, &applicationpb.Endorsements{
			EndorsementsWithIdentity: []*applicationpb.EndorsementWithIdentity{{Endorsement: sig}},
		})
	}

	return sets
}

// CreateEndorsementsForSignatureRule creates a EndorsementSet for a signature rule.
// It takes parallel slices of signatures, MSP IDs, and certificate bytes,
// and creates a EndorsementSet where each signature is paired with its corresponding
// identity (MSP ID and certificate). This is used when a set of signatures
// must all be present to satisfy a rule (e.g., an AND condition).
func CreateEndorsementsForSignatureRule(
	signatures, mspIDs, certBytesOrID [][]byte, creatorType int,
) *applicationpb.Endorsements {
	set := &applicationpb.Endorsements{
		EndorsementsWithIdentity: make([]*applicationpb.EndorsementWithIdentity, 0, len(signatures)),
	}
	for i, sig := range signatures {
		eid := &applicationpb.EndorsementWithIdentity{
			Endorsement: sig,
			Identity: &applicationpb.Identity{
				MspId: string(mspIDs[i]),
			},
		}
		switch creatorType {
		case test.CreatorCertificate:
			eid.Identity.Creator = &applicationpb.Identity_Certificate{Certificate: certBytesOrID[i]}
		case test.CreatorID:
			eid.Identity.Creator = &applicationpb.Identity_CertificateId{
				CertificateId: hex.EncodeToString(certBytesOrID[i]),
			}
		}

		set.EndorsementsWithIdentity = append(set.EndorsementsWithIdentity, eid)
	}
	return set
}
