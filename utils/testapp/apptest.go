/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package testapp

import (
	"context"
	"testing"

	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	"github.com/hyperledger/fabric-x-committer/utils/test"
)

// StatusRetriever provides implementation retrieve status of given transaction identifiers.
type StatusRetriever interface {
	GetTransactionsStatus(context.Context, *committerpb.TxIDsBatch, ...grpc.CallOption) (
		*committerpb.TxStatusBatch, error,
	)
}

// EnsurePersistedTxStatus fails the test if the given TX IDs does not match the expected status.
//
//nolint:revive,nolintlint // maximum number of arguments per function exceeded; max 4 but got 5.
func EnsurePersistedTxStatus(
	ctx context.Context,
	t *testing.T,
	r StatusRetriever,
	txIDs []string,
	expected []*committerpb.TxStatus,
) {
	t.Helper()
	if len(txIDs) == 0 {
		return
	}
	actualStatus, err := r.GetTransactionsStatus(ctx, &committerpb.TxIDsBatch{TxIds: txIDs})
	require.NoError(t, err)
	test.RequireProtoElementsMatch(t, expected, actualStatus.Status)
}

// RequireStatus fails if the expected status does not appear in the statuses list.
func RequireStatus(t require.TestingT, expected *committerpb.TxStatus, statuses []*committerpb.TxStatus) {
	var actualStatus *committerpb.TxStatus
	for _, status := range statuses {
		if status.Ref.TxId == expected.Ref.TxId {
			actualStatus = status
			break
		}
	}
	require.NotNil(t, actualStatus)
	test.RequireProtoEqual(t, expected, actualStatus)
}
