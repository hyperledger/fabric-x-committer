/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package apptest

import (
	"context"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	"github.com/hyperledger/fabric-x-committer/api/applicationpb"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

// StatusRetriever provides implementation retrieve status of given transaction identifiers.
type StatusRetriever interface {
	GetTransactionsStatus(context.Context, *applicationpb.QueryStatus, ...grpc.CallOption) (
		*applicationpb.TransactionsStatus, error,
	)
}

// EnsurePersistedTxStatus fails the test if the given TX IDs does not match the expected status.
//
//nolint:revive // maximum number of arguments per function exceeded; max 4 but got 5.
func EnsurePersistedTxStatus(
	ctx context.Context,
	t *testing.T,
	r StatusRetriever,
	txIDs []string,
	expected map[string]*applicationpb.StatusWithHeight,
) {
	t.Helper()
	if len(txIDs) == 0 {
		return
	}
	actualStatus, err := r.GetTransactionsStatus(ctx, &applicationpb.QueryStatus{TxIDs: txIDs})
	require.NoError(t, err)
	require.EqualExportedValues(t, expected, actualStatus.Status)
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

// AppendToEndorsementSetsForThresholdRule is a utility function that creates new signature sets
// for a threshold rule and appends them to an existing slice of signature sets.
func AppendToEndorsementSetsForThresholdRule(
	ss []*applicationpb.Endorsements, signatures ...[]byte,
) []*applicationpb.Endorsements {
	return append(ss, CreateEndorsementsForThresholdRule(signatures...)...)
}
