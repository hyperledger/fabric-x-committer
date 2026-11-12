/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package sidecar

import (
	"testing"

	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
)

func TestRelayRejectsMalformedResults(t *testing.T) {
	t.Parallel()
	for _, batch := range []*servicepb.TxStatusBatch{
		nil,
		{TxResult: []*servicepb.TxResult{nil}},
		{TxResult: []*servicepb.TxResult{{}}},
		{TxResult: []*servicepb.TxResult{{Status: &committerpb.TxStatus{}}}},
		{TxResult: []*servicepb.TxResult{{
			Status:             committerpb.NewTxStatus(committerpb.Status_ABORTED_MVCC_CONFLICT, "bad", 10, 0),
			BlindWriteVersions: []*committerpb.NamespaceWriteVersions{{NsId: "1"}},
		}}},
	} {
		incoming := make(chan *servicepb.TxStatusBatch, 1)
		incoming <- batch
		close(incoming)
		r := &relay{}
		require.Error(t, r.processStatusBatch(t.Context(), incoming))
	}
}
