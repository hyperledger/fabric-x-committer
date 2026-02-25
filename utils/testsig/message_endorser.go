/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package testsig

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/hyperledger/fabric-x-common/api/msppb"
	"github.com/hyperledger/fabric-x-common/msp"

	"github.com/hyperledger/fabric-x-committer/utils/signature"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

type (
	// endorser endorse a raw message.
	endorser interface {
		Endorse([]byte) (*applicationpb.Endorsements, error)
	}

	// mspEndorser endorse a raw message using identities loaded from MSP directories.
	mspEndorser struct {
		certType   int
		identities []msp.SigningIdentity
		mspIDs     [][]byte
		certsBytes [][]byte
	}

	// keyEndorser endorse a raw message using a signing key.
	keyEndorser struct {
		signer interface {
			Sign(signature.Digest) (signature.Signature, error)
		}
	}
)

// Endorse creates endorsements for all identities in the mspEndorser.
func (s *mspEndorser) Endorse(msg []byte) (*applicationpb.Endorsements, error) {
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

// Endorse endorses a digest of the given message.
func (d *keyEndorser) Endorse(msg []byte) (*applicationpb.Endorsements, error) {
	digest := sha256.Sum256(msg)
	sig, err := d.signer.Sign(digest[:])
	return CreateEndorsementsForThresholdRule(sig)[0], err
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
		}
		switch creatorType {
		case test.CreatorCertificate:
			eid.Identity = msppb.NewIdentity(string(mspIDs[i]), certBytesOrID[i])
		case test.CreatorID:
			eid.Identity = msppb.NewIdentityWithIDOfCert(string(mspIDs[i]), hex.EncodeToString(certBytesOrID[i]))
		}

		set.EndorsementsWithIdentity = append(set.EndorsementsWithIdentity, eid)
	}
	return set
}
