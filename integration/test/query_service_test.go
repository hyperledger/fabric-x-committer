/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"context"
	"testing"
	"time"

	"github.com/onsi/gomega"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/api/applicationpb"
	"github.com/hyperledger/fabric-x-committer/api/protonotify"
	"github.com/hyperledger/fabric-x-committer/api/protoqueryservice"
	"github.com/hyperledger/fabric-x-committer/integration/runner"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

func TestQueryService(t *testing.T) {
	t.Parallel()
	gomega.RegisterTestingT(t)
	c := runner.NewRuntime(t, &runner.Config{
		NumVerifiers: 2,
		NumVCService: 2,
		BlockTimeout: 2 * time.Second,
	})
	c.Start(t, runner.FullTxPathWithQuery)
	c.CreateNamespacesAndCommit(t, "1", "2")

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute*5)
	t.Cleanup(cancel)

	t.Log("Insert TXs")
	txIDs := c.MakeAndSendTransactionsToOrderer(t, [][]*applicationpb.TxNamespace{
		{{
			NsId:      "1",
			NsVersion: 0,
			BlindWrites: []*applicationpb.Write{
				{
					Key:   []byte("k1"),
					Value: []byte("v1"),
				},
				{
					Key:   []byte("k2"),
					Value: []byte("v2"),
				},
			},
		}},
		{{
			NsId:      "2",
			NsVersion: 0,
			BlindWrites: []*applicationpb.Write{
				{
					Key:   []byte("k3"),
					Value: []byte("v3"),
				},
				{
					Key:   []byte("k4"),
					Value: []byte("v4"),
				},
			},
		}},
	}, []applicationpb.Status{applicationpb.Status_COMMITTED, applicationpb.Status_COMMITTED})
	require.Len(t, txIDs, 2)

	t.Log("Query TXs status")
	status, err := c.QueryServiceClient.GetTransactionStatus(ctx, &protoqueryservice.TxStatusQuery{
		TxIds: txIDs,
	})
	require.NoError(t, err)
	require.Len(t, status.Statuses, len(txIDs))
	test.RequireProtoElementsMatch(t, []*protonotify.TxStatusEvent{
		{
			TxId: txIDs[0],
			StatusWithHeight: &applicationpb.StatusWithHeight{
				Code:        applicationpb.Status_COMMITTED,
				TxNumber:    uint32(0),
				BlockNumber: uint64(2),
			},
		},
		{
			TxId: txIDs[1],
			StatusWithHeight: &applicationpb.StatusWithHeight{
				Code:        applicationpb.Status_COMMITTED,
				TxNumber:    uint32(1),
				BlockNumber: uint64(2),
			},
		},
	}, status.Statuses)

	t.Log("Query Rows")
	ret, err := c.QueryServiceClient.GetRows(
		ctx,
		&protoqueryservice.Query{
			Namespaces: []*protoqueryservice.QueryNamespace{
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

	requiredItems := []*protoqueryservice.RowsNamespace{
		{
			NsId: "1",
			Rows: []*protoqueryservice.Row{
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
			Rows: []*protoqueryservice.Row{
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
	requiredItems []*protoqueryservice.RowsNamespace,
	retNamespaces []*protoqueryservice.RowsNamespace,
) {
	t.Helper()
	require.Len(t, retNamespaces, len(requiredItems))
	for idx := range retNamespaces {
		require.ElementsMatch(t, requiredItems[idx].Rows, retNamespaces[idx].Rows)
	}
}
