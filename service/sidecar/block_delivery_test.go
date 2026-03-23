/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package sidecar

import (
	"testing"

	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	ab "github.com/hyperledger/fabric-protos-go-apiv2/orderer"
	"github.com/hyperledger/fabric-protos-go-apiv2/peer"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/hyperledger/fabric-x-common/protoutil"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/hyperledger/fabric-x-committer/utils/test"
)

func TestBlockDelivery(t *testing.T) {
	t.Parallel()

	bs, _ := newBlockStoreWithBlocks(t, 3)

	// Register block delivery on a gRPC server.
	config := test.NewLocalHostServer(test.InsecureTLSConfig)
	test.RunGrpcServerForTest(t.Context(), t, config,
		func(server *grpc.Server) {
			peer.RegisterDeliverServer(server, newBlockDelivery(bs))
		})
	conn := test.NewInsecureConnection(t, &config.Endpoint)
	deliverClient := peer.NewDeliverClient(conn)

	t.Run("DeliverSpecificRange", func(t *testing.T) {
		t.Parallel()
		stream, err := deliverClient.Deliver(t.Context())
		require.NoError(t, err)

		env := seekEnvelope(t, 1, 2)
		require.NoError(t, stream.Send(env))

		// Should receive block 1, then block 2, then a SUCCESS status.
		for i := range 2 {
			resp, sErr := stream.Recv()
			require.NoError(t, sErr)
			require.Equal(t, uint64(i+1), resp.GetBlock().GetHeader().GetNumber()) //nolint:gosec // int -> uint64
		}

		statusResp, err := stream.Recv()
		require.NoError(t, err)
		require.Equal(t, common.Status_SUCCESS, statusResp.GetStatus())
	})

	t.Run("DeliverSingleBlock", func(t *testing.T) {
		t.Parallel()
		stream, err := deliverClient.Deliver(t.Context())
		require.NoError(t, err)

		env := seekEnvelope(t, 0, 0)
		require.NoError(t, stream.Send(env))

		resp, err := stream.Recv()
		require.NoError(t, err)
		require.Equal(t, uint64(0), resp.GetBlock().GetHeader().GetNumber())

		statusResp, err := stream.Recv()
		require.NoError(t, err)
		require.Equal(t, common.Status_SUCCESS, statusResp.GetStatus())
	})

	t.Run("DeliverFiltered", func(t *testing.T) {
		t.Parallel()
		stream, err := deliverClient.DeliverFiltered(t.Context())
		require.NoError(t, err)

		require.NoError(t, stream.Send(&common.Envelope{}))
		_, err = stream.Recv()
		require.Error(t, err)
		require.Equal(t, codes.Unimplemented, status.Code(err))
	})

	t.Run("DeliverWithPrivateData", func(t *testing.T) {
		t.Parallel()
		stream, err := deliverClient.DeliverWithPrivateData(t.Context())
		require.NoError(t, err)

		require.NoError(t, stream.Send(&common.Envelope{}))
		_, err = stream.Recv()
		require.Error(t, err)
		require.Equal(t, codes.Unimplemented, status.Code(err))
	})
}

// newBlockStoreWithBlocks creates a blockStore pre-populated with numBlocks chained blocks.
// Each block carries valid transaction metadata. Returns the blockStore and per-block txIDs.
func newBlockStoreWithBlocks(t *testing.T, numBlocks int) (*blockStore, [][3]string) {
	t.Helper()

	bs, err := newBlockStore(t.TempDir(), 0, newPerformanceMetrics())
	require.NoError(t, err)
	t.Cleanup(bs.close)

	valid := byte(committerpb.Status_COMMITTED)
	metadata := &common.BlockMetadata{
		Metadata: [][]byte{nil, nil, {valid, valid, valid}},
	}

	var prevHash []byte
	allTxIDs := make([][3]string, numBlocks)
	for i := range numBlocks {
		blk, txIDs := createBlockForTest(t, uint64(i), prevHash) //nolint:gosec
		blk.Metadata = metadata
		require.NoError(t, bs.ledger.Append(blk))
		prevHash = protoutil.BlockHeaderHash(blk.Header)
		allTxIDs[i] = txIDs
	}

	return bs, allTxIDs
}

// seekEnvelope creates a signed deliver envelope requesting blocks [start, stop].
func seekEnvelope(t *testing.T, start, stop uint64) *common.Envelope {
	t.Helper()
	env, err := protoutil.CreateSignedEnvelope(
		common.HeaderType_DELIVER_SEEK_INFO,
		"",
		nil, // no signer needed — the sidecar doesn't verify deliver request signatures.
		&ab.SeekInfo{
			Start:    &ab.SeekPosition{Type: &ab.SeekPosition_Specified{Specified: &ab.SeekSpecified{Number: start}}},
			Stop:     &ab.SeekPosition{Type: &ab.SeekPosition_Specified{Specified: &ab.SeekSpecified{Number: stop}}},
			Behavior: ab.SeekInfo_BLOCK_UNTIL_READY,
		},
		0, 0,
	)
	require.NoError(t, err)
	return env
}
