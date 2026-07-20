/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package sidecar

import (
	"context"
	"testing"
	"time"

	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	ab "github.com/hyperledger/fabric-protos-go-apiv2/orderer"
	"github.com/hyperledger/fabric-protos-go-apiv2/peer"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/hyperledger/fabric-x-common/protoutil"
	"github.com/hyperledger/fabric-x-common/utils/channel"
	"github.com/hyperledger/fabric-x-common/utils/connection"
	"github.com/hyperledger/fabric-x-common/utils/serve"
	commontest "github.com/hyperledger/fabric-x-common/utils/test"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/hyperledger/fabric-x-committer/utils/test"
)

type blockDeliveryWrapper struct {
	*Service
}

func (w *blockDeliveryWrapper) RegisterService(s serve.Servers) {
	peer.RegisterDeliverServer(s.GRPC, w)
}

func TestBlockDelivery(t *testing.T) {
	t.Parallel()

	bs, _ := newBlockStoreWithBlocks(t, 3)

	// Register block delivery on a gRPC server.
	serverConfig := commontest.NewLocalHostServiceConfig(commontest.InsecureTLSConfig)
	bd := &blockDeliveryWrapper{Service: &Service{blockStore: bs}}
	commontest.ServeForTest(t.Context(), t, serverConfig, bd)
	conn := commontest.NewInsecureConnection(t, &serverConfig.GRPC.Endpoint)
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
	return seekEnvelopeWithSeekInfo(t, &ab.SeekInfo{
		Start:    &ab.SeekPosition{Type: &ab.SeekPosition_Specified{Specified: &ab.SeekSpecified{Number: start}}},
		Stop:     &ab.SeekPosition{Type: &ab.SeekPosition_Specified{Specified: &ab.SeekSpecified{Number: stop}}},
		Behavior: ab.SeekInfo_BLOCK_UNTIL_READY,
	})
}

// seekEnvelopeWithSeekInfo creates a signed deliver envelope with the given SeekInfo.
func seekEnvelopeWithSeekInfo(t *testing.T, seekInfo *ab.SeekInfo) *common.Envelope {
	t.Helper()
	env, err := protoutil.CreateSignedEnvelope(
		common.HeaderType_DELIVER_SEEK_INFO,
		"",
		nil, // no signer needed — the sidecar doesn't verify deliver request signatures.
		seekInfo,
		0, 0,
	)
	require.NoError(t, err)
	return env
}

func TestBlockDeliveryWaitsForBlockZeroOnEmptyLedger(t *testing.T) {
	t.Parallel()

	type deliverResult struct {
		response *peer.DeliverResponse
		err      error
	}

	for _, tc := range []struct {
		name     string
		seekInfo *ab.SeekInfo
	}{
		{
			name: "newest stop",
			seekInfo: &ab.SeekInfo{
				Start:    &ab.SeekPosition{Type: &ab.SeekPosition_Specified{Specified: &ab.SeekSpecified{Number: 0}}},
				Stop:     &ab.SeekPosition{Type: &ab.SeekPosition_Newest{Newest: &ab.SeekNewest{}}},
				Behavior: ab.SeekInfo_BLOCK_UNTIL_READY,
			},
		},
		{
			name: "newest start and stop",
			seekInfo: &ab.SeekInfo{
				Start:    &ab.SeekPosition{Type: &ab.SeekPosition_Newest{Newest: &ab.SeekNewest{}}},
				Stop:     &ab.SeekPosition{Type: &ab.SeekPosition_Newest{Newest: &ab.SeekNewest{}}},
				Behavior: ab.SeekInfo_BLOCK_UNTIL_READY,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			metrics := newPerformanceMetrics()
			bs, err := newBlockStore(t.TempDir(), 0, metrics)
			require.NoError(t, err)
			t.Cleanup(bs.close)

			wrapper := &blockDeliveryWrapper{Service: &Service{blockStore: bs, metrics: metrics}}
			serverConfig := commontest.NewLocalHostServiceConfig(commontest.InsecureTLSConfig)
			commontest.ServeForTest(t.Context(), t, serverConfig, wrapper)
			conn := commontest.NewInsecureConnection(t, &serverConfig.GRPC.Endpoint)
			deliverClient := peer.NewDeliverClient(conn)

			streamCtx, streamCancel := context.WithCancel(t.Context())
			defer streamCancel()
			stream, err := deliverClient.Deliver(streamCtx)
			require.NoError(t, err)
			require.NoError(t, stream.Send(seekEnvelopeWithSeekInfo(t, tc.seekInfo)))

			resultCh := make(chan deliverResult, 1)
			go func() {
				resp, recvErr := stream.Recv()
				resultCh <- deliverResult{response: resp, err: recvErr}
			}()

			require.Never(t, func() bool {
				return len(resultCh) > 0
			}, time.Second, 250*time.Millisecond, "deliver returned before block 0 was available")

			inputBlock := make(chan *common.Block, 10)
			test.RunServiceForTest(t.Context(), t, func(ctx context.Context) error {
				return connection.FilterStreamRPCError(bs.run(ctx, &blockStoreRunConfig{
					IncomingCommittedBlock: inputBlock,
				}))
			}, nil)

			valid := byte(committerpb.Status_COMMITTED)
			metadata := &common.BlockMetadata{
				Metadata: [][]byte{nil, nil, {valid, valid, valid}},
			}
			blk, _ := createBlockForTest(t, 0, nil)
			blk.Metadata = metadata
			inputBlock <- blk
			ensureAtLeastHeight(t, bs, 1)

			result, ok := channel.NewReader(t.Context(), resultCh).ReadWithTimeout(2 * time.Second)
			require.True(t, ok)
			require.NoError(t, result.err)
			require.Equal(t, uint64(0), result.response.GetBlock().GetHeader().GetNumber())

			statusResp, err := stream.Recv()
			require.NoError(t, err)
			require.Equal(t, common.Status_SUCCESS, statusResp.GetStatus())
		})
	}
}
