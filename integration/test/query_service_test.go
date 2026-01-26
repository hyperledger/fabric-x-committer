/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"context"
	"testing"
	"time"

	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/onsi/gomega"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/hyperledger/fabric-x-committer/integration/runner"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

func TestQueryService(t *testing.T) {
	t.Parallel()
	c, ctx, txIDs := setupQueryService(t, 0)

	t.Log("Query TXs status")
	status, err := c.QueryServiceClient.GetTransactionStatus(ctx, &committerpb.TxStatusQuery{
		TxIds: txIDs,
	})
	require.NoError(t, err)
	require.Len(t, status.Statuses, len(txIDs))
	test.RequireProtoElementsMatch(t, []*committerpb.TxStatus{
		{
			Ref:    committerpb.NewTxRef(txIDs[0], 2, 0),
			Status: committerpb.Status_COMMITTED,
		},
		{
			Ref:    committerpb.NewTxRef(txIDs[1], 2, 1),
			Status: committerpb.Status_COMMITTED,
		},
	}, status.Statuses)

	t.Log("Query Rows")
	ret, err := c.QueryServiceClient.GetRows(
		ctx,
		&committerpb.Query{
			Namespaces: []*committerpb.QueryNamespace{
				{
					NsId: "1",
					Keys: [][]byte{
						[]byte("k1"), []byte("k2"),
					},
				},
				{
					NsId: "2",
					Keys: [][]byte{
						[]byte("k3"), []byte("k4"),
					},
				},
			},
		},
	)
	require.NoError(t, err)

	testItemsVersion := uint64(0)

	requiredItems := []*committerpb.RowsNamespace{
		{
			NsId: "1",
			Rows: []*committerpb.Row{
				{
					Key:     []byte("k1"),
					Value:   []byte("v1"),
					Version: testItemsVersion,
				},
				{
					Key:     []byte("k2"),
					Value:   []byte("v2"),
					Version: testItemsVersion,
				},
			},
		},
		{
			NsId: "2",
			Rows: []*committerpb.Row{
				{
					Key:     []byte("k3"),
					Value:   []byte("v3"),
					Version: testItemsVersion,
				},
				{
					Key:     []byte("k4"),
					Value:   []byte("v4"),
					Version: testItemsVersion,
				},
			},
		},
	}

	requireQueryResults(
		t,
		requiredItems,
		ret.Namespaces,
	)
}

// requireQueryResults requires that the items retrieved by the Query service
// equals to the test items that added to the DB.
func requireQueryResults(
	t *testing.T,
	requiredItems []*committerpb.RowsNamespace,
	retNamespaces []*committerpb.RowsNamespace,
) {
	t.Helper()
	require.Len(t, retNamespaces, len(requiredItems))
	for idx := range retNamespaces {
		require.ElementsMatch(t, requiredItems[idx].Rows, retNamespaces[idx].Rows)
	}
}

func TestQueryServiceMaxRequestKeys(t *testing.T) {
	t.Parallel()
	c, ctx, txIDs := setupQueryService(t, 3)

	t.Run("GetRows within limit succeeds", func(t *testing.T) {
		t.Parallel()
		// Request 3 keys, which is exactly at the limit.
		ret, err := c.QueryServiceClient.GetRows(ctx, &committerpb.Query{
			Namespaces: []*committerpb.QueryNamespace{
				{NsId: "1", Keys: [][]byte{[]byte("k1"), []byte("k2")}},
				{NsId: "2", Keys: [][]byte{[]byte("k3")}},
			},
		})
		require.NoError(t, err)
		require.Len(t, ret.Namespaces, 2)
	})

	t.Run("GetRows exceeds limit returns error", func(t *testing.T) {
		t.Parallel()
		// Request 4 keys across namespaces, exceeding the limit of 3.
		_, err := c.QueryServiceClient.GetRows(ctx, &committerpb.Query{
			Namespaces: []*committerpb.QueryNamespace{
				{NsId: "1", Keys: [][]byte{[]byte("k1"), []byte("k2")}},
				{NsId: "2", Keys: [][]byte{[]byte("k3"), []byte("k4")}},
			},
		})
		require.Error(t, err)
		st, ok := status.FromError(err)
		require.True(t, ok)
		require.Equal(t, codes.InvalidArgument, st.Code())
		require.Contains(t, st.Message(), "request exceeds maximum allowed keys")
	})

	t.Run("GetTransactionStatus within limit succeeds", func(t *testing.T) {
		t.Parallel()
		// Request 2 transaction IDs, which is within the limit of 3.
		ret, err := c.QueryServiceClient.GetTransactionStatus(ctx, &committerpb.TxStatusQuery{
			TxIds: txIDs,
		})
		require.NoError(t, err)
		require.Len(t, ret.Statuses, len(txIDs))
	})

	t.Run("GetTransactionStatus exceeds limit returns error", func(t *testing.T) {
		t.Parallel()
		// Request 4 transaction IDs, exceeding the limit of 3.
		_, err := c.QueryServiceClient.GetTransactionStatus(ctx, &committerpb.TxStatusQuery{
			TxIds: []string{"tx1", "tx2", "tx3", "tx4"},
		})
		require.Error(t, err)
		st, ok := status.FromError(err)
		require.True(t, ok)
		require.Equal(t, codes.InvalidArgument, st.Code())
		require.Contains(t, st.Message(), "request exceeds maximum allowed keys")
	})
}

func setupQueryService(
	t *testing.T,
	maxRequestKeys int,
) (c *runner.CommitterRuntime, ctx context.Context, txIDs []string) {
	t.Helper()
	gomega.RegisterTestingT(t)

	c = runner.NewRuntime(t, &runner.Config{
		NumVerifiers:   2,
		NumVCService:   2,
		BlockTimeout:   2 * time.Second,
		MaxRequestKeys: maxRequestKeys,
	})
	c.Start(t, runner.FullTxPathWithQuery)
	c.CreateNamespacesAndCommit(t, "1", "2")

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute*5)
	t.Cleanup(cancel)

	t.Log("Insert TXs")
	txIDs = c.MakeAndSendTransactionsToOrderer(t,
		[][]*applicationpb.TxNamespace{
			{{
				NsId:      "1",
				NsVersion: 0,
				BlindWrites: []*applicationpb.Write{
					{Key: []byte("k1"), Value: []byte("v1")},
					{Key: []byte("k2"), Value: []byte("v2")},
				},
			}},
			{{
				NsId:      "2",
				NsVersion: 0,
				BlindWrites: []*applicationpb.Write{
					{Key: []byte("k3"), Value: []byte("v3")},
					{Key: []byte("k4"), Value: []byte("v4")},
				},
			}},
		},
		[]committerpb.Status{committerpb.Status_COMMITTED, committerpb.Status_COMMITTED},
	)
	require.Len(t, txIDs, 2)

	return c, ctx, txIDs
}
