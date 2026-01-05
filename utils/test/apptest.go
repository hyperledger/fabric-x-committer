/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
)

// StatusRetriever provides implementation retrieve status of given transaction identifiers.
type StatusRetriever interface {
	GetTransactionsStatus(context.Context, *servicepb.QueryStatus, ...grpc.CallOption) (
		*servicepb.TransactionsStatus, error,
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
	expected map[string]*servicepb.StatusWithHeight,
) {
	t.Helper()
	if len(txIDs) == 0 {
		return
	}
	actualStatus, err := r.GetTransactionsStatus(ctx, &servicepb.QueryStatus{TxIDs: txIDs})
	require.NoError(t, err)
	require.EqualExportedValues(t, expected, actualStatus.Status)
}
