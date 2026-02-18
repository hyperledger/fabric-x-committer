/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package sidecar

import (
	"testing"

	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

func TestBlockQuery(t *testing.T) {
	t.Parallel()

	bs, txIDs := newBlockStoreWithBlocks(t, "ch1", 2)

	// Create the query service and register on a gRPC server.
	queryService := newBlockQuery(bs.store)

	config := connection.NewLocalHostServer(test.InsecureTLSConfig)
	test.RunGrpcServerForTest(t.Context(), t, config, func(server *grpc.Server) {
		committerpb.RegisterBlockQueryServiceServer(server, queryService)
	})

	conn := test.NewInsecureConnection(t, &config.Endpoint)
	client := committerpb.NewBlockQueryServiceClient(conn)

	t.Run("GetBlockchainInfo", func(t *testing.T) {
		t.Parallel()
		info, err := client.GetBlockchainInfo(t.Context(), &emptypb.Empty{})
		require.NoError(t, err)
		require.Equal(t, uint64(2), info.GetHeight())
	})

	t.Run("GetBlockByNumber", func(t *testing.T) {
		t.Parallel()
		block, err := client.GetBlockByNumber(t.Context(), &committerpb.BlockNumber{Number: 1})
		require.NoError(t, err)
		require.Equal(t, uint64(1), block.GetHeader().GetNumber())
	})

	t.Run("GetBlockByNumber_NotFound", func(t *testing.T) {
		t.Parallel()
		_, err := client.GetBlockByNumber(t.Context(), &committerpb.BlockNumber{Number: 999})
		require.Error(t, err)
		require.Equal(t, codes.NotFound, status.Code(err))
	})

	t.Run("GetBlockByTxID", func(t *testing.T) {
		t.Parallel()
		block, err := client.GetBlockByTxID(t.Context(), &committerpb.TxID{TxId: txIDs[1][0]})
		require.NoError(t, err)
		require.Equal(t, uint64(1), block.GetHeader().GetNumber())
	})

	t.Run("GetBlockByTxID_NotFound", func(t *testing.T) {
		t.Parallel()
		_, err := client.GetBlockByTxID(t.Context(), &committerpb.TxID{TxId: "nonexistent"})
		require.Error(t, err)
		require.Equal(t, codes.NotFound, status.Code(err))
	})

	t.Run("GetTxByID", func(t *testing.T) {
		t.Parallel()
		envelope, err := client.GetTxByID(t.Context(), &committerpb.TxID{TxId: txIDs[0][0]})
		require.NoError(t, err)
		require.NotNil(t, envelope)
	})

	t.Run("GetTxByID_NotFound", func(t *testing.T) {
		t.Parallel()
		_, err := client.GetTxByID(t.Context(), &committerpb.TxID{TxId: "nonexistent"})
		require.Error(t, err)
		require.Equal(t, codes.NotFound, status.Code(err))
	})
}
