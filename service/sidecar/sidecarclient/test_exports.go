/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package sidecarclient

import (
	"context"
	"testing"

	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

// StartSidecarClient starts a deliver client to fetch committed blocks from the sidecar/ledger service.
func StartSidecarClient(
	ctx context.Context,
	t *testing.T,
	config *Parameters,
	startBlkNum uint64,
) chan *common.Block {
	t.Helper()
	receivedBlocksFromLedgerService := make(chan *common.Block, 10)
	deliverClient, err := New(config)
	require.NoError(t, err)
	test.RunServiceForTest(ctx, t, func(ctx context.Context) error {
		return connection.FilterStreamRPCError(deliverClient.Deliver(ctx,
			&DeliverParameters{
				NextBlockNum: startBlkNum,
				OutputBlock:  receivedBlocksFromLedgerService,
			},
		))
	}, nil)
	return receivedBlocksFromLedgerService
}
