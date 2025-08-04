/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package mock

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric/protoutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

func TestAddEnvelopeToCache(t *testing.T) {
	t.Parallel()
	c := newFifoCache[any](10)
	for i := range 10 {
		require.Truef(t, addEnvelope(c, makeEnv(i)), "insert %d", i)
	}
	for i := range 10 {
		require.Falsef(t, addEnvelope(c, makeEnv(i)), "insert %d", i)
	}

	for i := range 3 {
		require.Truef(t, addEnvelope(c, makeEnv(i+10)), "insert %d", i)
	}
	for i := range 3 {
		require.Truef(t, addEnvelope(c, makeEnv(i)), "insert %d", i)
	}
	for i := range 4 {
		require.Falsef(t, addEnvelope(c, makeEnv(i+6)), "insert %d", i)
	}
}

func TestBlockCache(t *testing.T) {
	t.Parallel()
	c := newBlockCache(10)

	ctx := releaseCacheAfterTimeout(t.Context(), t, c, 3*time.Minute)

	// insert all.
	for i := range 10 {
		require.Truef(t, c.addBlock(ctx, makeBlockNumber(i)), "insert %d", i)
	}

	// insert wait and fail.
	waitCtx := releaseCacheAfterTimeout(ctx, t, c, 3*time.Second)
	require.False(t, c.addBlock(waitCtx, makeBlockNumber(10)), "insert %d", 10)

	// insert wait and success.
	addBlockSuccess := make(chan bool)
	go func() {
		defer close(addBlockSuccess)
		channel.NewWriter(ctx, addBlockSuccess).Write(c.addBlock(ctx, makeBlockNumber(10)))
	}()

	waitCtx = releaseCacheAfterTimeout(ctx, t, c, 3*time.Second)
	_, ok := channel.NewReader(waitCtx, addBlockSuccess).Read()
	require.False(t, ok)

	block, err := c.getBlock(ctx, 0)
	require.NoError(t, err)
	require.NotNil(t, block)
	require.Equal(t, uint64(0), block.Header.Number)

	success, ok := channel.NewReader(ctx, addBlockSuccess).Read()
	require.True(t, ok)
	require.True(t, success)

	// get all.
	block, err = c.getBlock(ctx, 0)
	require.ErrorIs(t, err, ErrLostBlock)
	require.Nil(t, block)

	for i := range 10 {
		block, err = c.getBlock(ctx, uint64(i+1)) //nolint:gosec // integer overflow conversion int -> uint64
		require.NoError(t, err)
		require.NotNil(t, block)
		//nolint:gosec // integer overflow conversion int -> uint64
		require.Equal(t, uint64(i+1), block.Header.Number)
	}

	// get wait and fail.
	waitCtx = releaseCacheAfterTimeout(ctx, t, c, 3*time.Second)
	block, err = c.getBlock(waitCtx, 11)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Nil(t, block)

	// get wait and success
	getBlockResult := make(chan *common.Block)
	go func() {
		defer close(getBlockResult)
		b, err := c.getBlock(ctx, 11)
		assert.NoError(t, err)
		channel.NewWriter(ctx, getBlockResult).Write(b)
	}()

	waitCtx = releaseCacheAfterTimeout(ctx, t, c, 3*time.Second)
	_, ok = channel.NewReader(waitCtx, getBlockResult).Read()
	require.False(t, ok)

	require.True(t, c.addBlock(ctx, makeBlockNumber(11)))

	block, ok = channel.NewReader(ctx, getBlockResult).Read()
	require.True(t, ok)
	require.NotNil(t, block)
	require.Equal(t, uint64(11), block.Header.Number)
}

// TestOrderer tests just the testing functionality.
// It does not validate the streaming API.
func TestOrderer(t *testing.T) {
	t.Parallel()
	o, err := NewMockOrderer(&OrdererConfig{
		BlockSize:        3,
		BlockTimeout:     time.Hour,
		OutBlockCapacity: 10,
		PayloadCacheSize: 10,
		SendConfigBlock:  true,
	})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	t.Cleanup(cancel)
	test.RunServiceForTest(ctx, t, func(ctx context.Context) error {
		return connection.FilterStreamRPCError(o.Run(ctx))
	}, o.WaitForReady)

	var expectedBlock uint64

	// config block.
	block, err := o.GetBlock(ctx, expectedBlock)
	require.NoError(t, err)
	require.NotNil(t, block)
	require.Equal(t, expectedBlock, block.Header.Number)
	require.Equal(t, defaultConfigBlock.Data.Data, block.Data.Data)

	expectedBlock++

	// get wait and fail
	waitCtx, waitCancel := context.WithTimeout(ctx, 3*time.Second)
	t.Cleanup(waitCancel)
	block, err = o.GetBlock(waitCtx, expectedBlock)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Nil(t, block)

	// insert block and get it.
	sentBlock := makeBlockData(expectedBlock)
	require.NoError(t, o.SubmitBlock(ctx, sentBlock))

	block, err = o.GetBlock(ctx, expectedBlock)
	require.NoError(t, err)
	require.NotNil(t, block)
	require.Equal(t, expectedBlock, block.Header.Number)
	require.Equal(t, sentBlock.Data.Data, block.Data.Data)

	expectedBlock++

	// submit env and force cut
	sentEnv := makeEnvelopePayload(1)
	require.True(t, o.SubmitEnv(ctx, sentEnv))

	require.True(t, o.CutBlock(ctx))

	block, err = o.GetBlock(ctx, expectedBlock)
	require.NoError(t, err)
	require.NotNil(t, block)
	require.Equal(t, expectedBlock, block.Header.Number)
	require.Len(t, block.Data.Data, 1)
	env, err := protoutil.UnmarshalEnvelope(block.Data.Data[0])
	require.NoError(t, err)
	require.Equal(t, sentEnv.Payload, env.Payload)

	expectedBlock++

	// submit env and force cut
	sentEnv1 := makeEnvelopePayload(2)
	require.True(t, o.SubmitEnv(ctx, sentEnv1))

	require.True(t, o.CutBlock(ctx))

	block, err = o.GetBlock(ctx, expectedBlock)
	require.NoError(t, err)
	require.NotNil(t, block)
	require.Equal(t, expectedBlock, block.Header.Number)
	require.Len(t, block.Data.Data, 1)
	env1, err := protoutil.UnmarshalEnvelope(block.Data.Data[0])
	require.NoError(t, err)
	require.Equal(t, sentEnv1.Payload, env1.Payload)

	expectedBlock++

	// size cut
	ctx, cancel = context.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(cancel)
	for i := range 3 {
		require.True(t, o.SubmitEnv(ctx, makeEnvelopePayload(i+10)))
	}

	block, err = o.GetBlock(ctx, expectedBlock)
	require.NoError(t, err)
	require.NotNil(t, block)
	require.Equal(t, expectedBlock, block.Header.Number)
	require.Len(t, block.Data.Data, 3)
	for i, data := range block.Data.Data {
		e, uErr := protoutil.UnmarshalEnvelope(data)
		require.NoError(t, uErr)
		require.Equal(t, makeEnvelopePayload(i+10).Payload, e.Payload)
	}

	expectedBlock++

	// timeout cut
	require.True(t, o.SubmitEnv(ctx, makeEnvelopePayload(20)))

	waitCtx, waitCancel = context.WithTimeout(ctx, 3*time.Second)
	t.Cleanup(waitCancel)
	block, err = o.GetBlock(waitCtx, expectedBlock)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Nil(t, block)

	// We manually override this config, so it will be used after the next cut.
	o.config.BlockTimeout = time.Second

	require.True(t, o.CutBlock(ctx))
	// We ignore one block.
	expectedBlock++

	require.True(t, o.SubmitEnv(ctx, makeEnvelopePayload(30)))

	block, err = o.GetBlock(ctx, expectedBlock)
	require.NoError(t, err)
	require.NotNil(t, block)
	require.Equal(t, expectedBlock, block.Header.Number)
	require.Len(t, block.Data.Data, 1)
	e, err := protoutil.UnmarshalEnvelope(block.Data.Data[0])
	require.NoError(t, err)
	require.Equal(t, makeEnvelopePayload(30).Payload, e.Payload)
}

func releaseCacheAfterTimeout(
	parentCtx context.Context, t *testing.T, cache *blockCache, timeout time.Duration,
) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(parentCtx, timeout)
	t.Cleanup(cancel)
	stop := cache.releaseAfter(ctx)
	t.Cleanup(func() { stop() })
	return ctx
}

func makeEnv(i int) *common.Envelope {
	return &common.Envelope{
		Payload:   []byte(fmt.Sprintf("%d", i)),
		Signature: []byte("sig"),
	}
}

func makeBlockNumber(i int) *common.Block {
	return &common.Block{
		Header: &common.BlockHeader{
			Number: uint64(i), //nolint:gosec // integer overflow conversion int -> uint64
		},
	}
}

func makeBlockData(i uint64) *common.Block {
	return &common.Block{
		Data: &common.BlockData{
			Data: [][]byte{[]byte(fmt.Sprintf("%d", i))},
		},
	}
}

func makeEnvelopePayload(i int) *common.Envelope {
	return &common.Envelope{
		Payload: []byte(fmt.Sprintf("%d", i)),
	}
}
