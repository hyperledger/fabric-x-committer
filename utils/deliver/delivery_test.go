/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package deliver

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/deliver/mocks"
)

//go:generate mockgen -source=delivery.go -destination=mocks/streamer.mock.go -package=mocks

func TestDeliverToChannel(t *testing.T) {
	t.Parallel()

	e := newDeliveryTestEnv(t)
	s := mocks.NewMockStreamer(e.ctrl)

	t.Log("Reading seek env")
	s.EXPECT().Send(gomock.Not(gomock.Nil())).Times(1).Return(nil)
	nextBlockNum := uint64(0)
	// We expect .RecvBlockOrStatus() to be called up to 21 times.
	// 10 times for the pulled blocks, 10 times for the blocks in the buffer, and 1 that blocks because
	// the buffer is full.
	s.EXPECT().RecvBlockOrStatus().MaxTimes(21).DoAndReturn(func() (*common.Block, *common.Status, error) {
		b := &common.Block{Header: &common.BlockHeader{Number: nextBlockNum}}
		nextBlockNum++
		return b, nil, nil
	})
	channel.NewWriter(t.Context(), e.streamer).Write(s)

	t.Log("Reading the block from the delivery output")
	outputBlock := channel.NewReader(t.Context(), e.outputBlock)
	outputBlockWithSourceID := channel.NewReader(t.Context(), e.outputBlockWithSourceID)
	for i := range uint64(10) {
		expectedBlock := &common.Block{Header: &common.BlockHeader{Number: i}}

		readBlock, ok := outputBlock.ReadWithTimeout(3 * time.Second)
		require.True(t, ok)
		require.Equal(t, expectedBlock, readBlock)

		readBlockWithSourceID, ok := outputBlockWithSourceID.ReadWithTimeout(3 * time.Second)
		require.True(t, ok)
		require.NotNil(t, readBlockWithSourceID)
		require.Equal(t, expectedBlock, readBlockWithSourceID.Block)
		require.EqualValues(t, 1, readBlockWithSourceID.SourceID)
	}
}

func TestDeliverToChannelFailedStreamer(t *testing.T) {
	t.Parallel()

	e := newDeliveryTestEnv(t)

	t.Log("Cannot create stream, so never receive block")
	streamerQueue := channel.NewWriter(t.Context(), e.streamer)
	streamerQueue.Write(nil)
	streamerQueue.Write(nil)
	streamerQueue.Write(nil)
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		require.Empty(ct, e.streamer)
	}, 3*time.Second, 10*time.Millisecond)
	outputBlock := channel.NewReader(t.Context(), e.outputBlock)
	_, ok := outputBlock.ReadWithTimeout(time.Second)
	require.False(t, ok)

	t.Log("Failed to send seek, so never receive block")
	s := mocks.NewMockStreamer(e.ctrl)
	s.EXPECT().Send(gomock.Not(gomock.Nil())).Times(1).Return(errors.New("failed seek"))
	s.EXPECT().RecvBlockOrStatus().Times(0)
	streamerQueue.Write(s)
	_, ok = outputBlock.ReadWithTimeout(time.Second)
	require.False(t, ok)

	// Seek always succeed from now on.
	s.EXPECT().Send(gomock.Not(gomock.Nil())).AnyTimes().Return(nil)

	t.Log("Failed receive block (error)")
	s = mocks.NewMockStreamer(e.ctrl)
	s.EXPECT().Send(gomock.Not(gomock.Nil())).Times(1).Return(nil)
	s.EXPECT().RecvBlockOrStatus().Times(1).Return(nil, nil, errors.New("failed block"))
	streamerQueue.Write(s)
	_, ok = outputBlock.ReadWithTimeout(time.Second)
	require.False(t, ok)

	t.Log("Failed receive block (status)")
	s = mocks.NewMockStreamer(e.ctrl)
	s.EXPECT().Send(gomock.Not(gomock.Nil())).Times(1).Return(nil)
	s.EXPECT().RecvBlockOrStatus().Times(1).Return(nil, new(common.Status_NOT_FOUND), nil)
	streamerQueue.Write(s)
	_, ok = outputBlock.ReadWithTimeout(time.Second)
	require.False(t, ok)

	t.Log("Receive bad block (nil header)")
	s = mocks.NewMockStreamer(e.ctrl)
	s.EXPECT().Send(gomock.Not(gomock.Nil())).Times(1).Return(nil)
	s.EXPECT().RecvBlockOrStatus().Times(1).Return(&common.Block{}, nil, nil)
	streamerQueue.Write(s)
	_, ok = outputBlock.ReadWithTimeout(time.Second)
	require.False(t, ok)

	t.Log("Receive bad block (wrong number 1 != 0)")
	s = mocks.NewMockStreamer(e.ctrl)
	s.EXPECT().Send(gomock.Not(gomock.Nil())).Times(1).Return(nil)
	s.EXPECT().RecvBlockOrStatus().Times(1).Return(&common.Block{Header: &common.BlockHeader{Number: 1}}, nil, nil)
	streamerQueue.Write(s)
	_, ok = outputBlock.ReadWithTimeout(time.Second)
	require.False(t, ok)

	t.Log("Correct block, then bad block (wrong number 0 != 1)")
	s = mocks.NewMockStreamer(e.ctrl)
	s.EXPECT().Send(gomock.Not(gomock.Nil())).Times(1).Return(nil)
	correctBlock := &common.Block{Header: &common.BlockHeader{Number: 0}}
	s.EXPECT().RecvBlockOrStatus().Times(2).Return(correctBlock, nil, nil)
	streamerQueue.Write(s)
	rb, ok := outputBlock.ReadWithTimeout(3 * time.Second)
	require.True(t, ok)
	require.Equal(t, correctBlock, rb)
	_, ok = outputBlock.ReadWithTimeout(time.Second)
	require.False(t, ok)
}

type deliveryTestEnv struct {
	ctrl                    *gomock.Controller
	streamer                chan *mocks.MockStreamer
	outputBlock             chan *common.Block
	outputBlockWithSourceID chan *BlockWithSourceID
}

func newDeliveryTestEnv(t *testing.T) *deliveryTestEnv {
	t.Helper()
	ctrl := gomock.NewController(t)
	t.Cleanup(ctrl.Finish)

	e := &deliveryTestEnv{
		ctrl:                    ctrl,
		streamer:                make(chan *mocks.MockStreamer, 10),
		outputBlock:             make(chan *common.Block, 10),
		outputBlockWithSourceID: make(chan *BlockWithSourceID, 10),
	}

	wg := sync.WaitGroup{}
	t.Cleanup(wg.Wait)
	wg.Go(func() {
		err := ToQueue(t.Context(), Parameters{
			StreamCreator: func(ctx context.Context) (Streamer, error) {
				s, ok := channel.NewReader(ctx, e.streamer).Read()
				if !ok {
					return nil, errors.New("context ended")
				}
				if s == nil {
					return nil, errors.New("bad connection")
				}
				s.EXPECT().Context().MinTimes(1).Return(ctx)
				return s, nil
			},
			ChannelID:               "channelID",
			OutputBlock:             e.outputBlock,
			OutputBlockWithSourceID: e.outputBlockWithSourceID,
			SourceID:                1,
			Retry: &connection.RetryProfile{
				// We set max interval for 1 second to ensure test progress.
				MaxInterval: time.Second,
			},
		})
		assert.ErrorIs(t, err, context.Canceled)
	})

	return e
}
