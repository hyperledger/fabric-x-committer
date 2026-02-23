/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package mock

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	ab "github.com/hyperledger/fabric-protos-go-apiv2/orderer"
	"github.com/hyperledger/fabric-x-common/protoutil"
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

	t.Log("Insert all")
	for i := range 10 {
		require.Truef(t, c.addBlock(ctx, makeBlockNumber(i)), "insert %d", i)
	}

	t.Log("Insert wait and fail")
	waitCtx := releaseCacheAfterTimeout(ctx, t, c, 3*time.Second)
	require.False(t, c.addBlock(waitCtx, makeBlockNumber(10)), "insert %d", 10)

	t.Log("Insert wait and success")
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

	t.Log("Get all")
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

	t.Log("Get wait and fail")
	waitCtx = releaseCacheAfterTimeout(ctx, t, c, 3*time.Second)
	block, err = c.getBlock(waitCtx, 11)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Nil(t, block)

	t.Log("Get wait and success")
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

	t.Log("Config block")
	block, err := o.GetBlock(ctx, expectedBlock)
	require.NoError(t, err)
	require.NotNil(t, block)
	require.Equal(t, expectedBlock, block.Header.Number)
	require.Equal(t, defaultConfigBlock.Data.Data, block.Data.Data)

	expectedBlock++

	t.Log("Get wait and fail")
	waitCtx, waitCancel := context.WithTimeout(ctx, 3*time.Second)
	t.Cleanup(waitCancel)
	block, err = o.GetBlock(waitCtx, expectedBlock)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Nil(t, block)

	t.Log("Insert block and get it")
	sentBlock := makeBlockData(expectedBlock)
	require.NoError(t, o.SubmitBlock(ctx, sentBlock))

	block, err = o.GetBlock(ctx, expectedBlock)
	require.NoError(t, err)
	require.NotNil(t, block)
	require.Equal(t, expectedBlock, block.Header.Number)
	require.Equal(t, sentBlock.Data.Data, block.Data.Data)

	expectedBlock++

	t.Log("Submit env and force cut")
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

	t.Log("Submit env and force cut")
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

	t.Log("Size cut")
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

	t.Log("Timeout cut")
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

// TestOrdererStreamingAPI tests the streaming API and the stream state registry.
func TestOrdererStreamingAPI(t *testing.T) {
	t.Parallel()
	o, err := NewMockOrderer(&OrdererConfig{
		BlockSize:       1,
		SendConfigBlock: false,
	})
	require.NoError(t, err)
	require.NotNil(t, o)
	test.RunServiceForTest(t.Context(), t, func(ctx context.Context) error {
		return connection.FilterStreamRPCError(o.Run(ctx))
	}, o.WaitForReady)

	numServices := 3
	s := test.StartGrpcServersForTest(t.Context(), t, test.StartServerParameters{
		NumService: numServices,
	}, o.RegisterService)
	require.NotNil(t, s)
	require.Len(t, s.Configs, numServices)
	require.Len(t, s.Servers, numServices)

	t.Log("Start with no streams")
	RequireStreams(t, o, 0)

	ep0 := s.Configs[0].Endpoint
	ep1 := s.Configs[1].Endpoint
	ep2 := s.Configs[2].Endpoint
	addr0 := ep0.Address()
	addr1 := ep1.Address()
	addr2 := ep2.Address()

	t.Log("Open streams - 1 per endpoint")
	s0 := startStream(t, ep0)
	RequireStreams(t, o, 1)
	RequireStreamsWithEndpoints(t, o, 1, addr0)

	s1 := startStream(t, ep1)
	RequireStreams(t, o, 2)
	RequireStreamsWithEndpoints(t, o, 1, addr1)

	s2 := startStream(t, ep2)
	RequireStreams(t, o, 3)
	RequireStreamsWithEndpoints(t, o, 1, addr2)

	t.Log("Open a second stream for endpoint")
	s3 := startStream(t, ep2)
	RequireStreams(t, o, 4)
	RequireStreamsWithEndpoints(t, o, 2, addr2)

	t.Log("Close streams")
	s0.cancel()
	RequireStreams(t, o, 3)
	RequireStreamsWithEndpoints(t, o, 0, addr0)

	s1.cancel()
	RequireStreams(t, o, 2)
	RequireStreamsWithEndpoints(t, o, 0, addr1)

	s2.cancel()
	RequireStreams(t, o, 1)
	RequireStreamsWithEndpoints(t, o, 1, addr2)

	s3.cancel()
	RequireStreams(t, o, 0)
	RequireStreamsWithEndpoints(t, o, 0, addr2)

	t.Log("Register parties")
	p1 := PartyState{PartyID: 1}
	p2 := PartyState{PartyID: 2}
	o.RegisterPartyState(addr0, &p1)
	o.RegisterPartyState(addr1, &p1)
	o.RegisterPartyState(addr2, &p2)

	s0 = startStream(t, ep0)
	streamState0 := RequireStreamsWithEndpoints(t, o, 1, addr0)
	require.Len(t, streamState0, 1)
	require.EqualValues(t, 1, streamState0[0].PartyID)

	s1 = startStream(t, ep1)
	streamState1 := RequireStreamsWithEndpoints(t, o, 1, addr1)
	require.Len(t, streamState1, 1)
	require.EqualValues(t, 1, streamState1[0].PartyID)

	s2 = startStream(t, ep2)
	streamState2 := RequireStreamsWithEndpoints(t, o, 1, addr2)
	require.Len(t, streamState2, 1)
	require.EqualValues(t, 2, streamState2[0].PartyID)

	t.Log("Send and receive block (sanity check)")
	channel.NewWriter(t.Context(), s0.input).Write(&common.Envelope{Payload: []byte{1}})
	for _, stream := range []*streamTestData{s0, s1, s2} {
		b, ok := channel.NewReader(t.Context(), stream.output).ReadWithTimeout(10 * time.Second)
		require.True(t, ok)
		require.NotNil(t, b)
	}

	t.Log("Hold party 1")
	p1.HoldFromBlock.Store(1)
	channel.NewWriter(t.Context(), s1.input).Write(&common.Envelope{Payload: []byte{2}})
	b, ok := channel.NewReader(t.Context(), s2.output).ReadWithTimeout(10 * time.Second)
	require.True(t, ok)
	require.NotNil(t, b)
	select {
	case <-t.Context().Done():
		t.Fatal("Context ended")
	case <-time.After(10 * time.Second):
		t.Log("Fantastic")
	case <-s0.output:
		t.Fatal("Party 1 should not have receive the block")
	case <-s1.output:
		t.Fatal("Party 1 should not have receive the block")
	}

	t.Log("Release party 1 and hold party 2")
	p1.HoldFromBlock.Store(0)
	for _, stream := range []*streamTestData{s0, s1} {
		b, ok = channel.NewReader(t.Context(), stream.output).ReadWithTimeout(10 * time.Second)
		require.True(t, ok)
		require.NotNil(t, b)
	}

	p2.HoldFromBlock.Store(2)
	channel.NewWriter(t.Context(), s2.input).Write(&common.Envelope{Payload: []byte{3}})
	for _, stream := range []*streamTestData{s0, s1} {
		b, ok = channel.NewReader(t.Context(), stream.output).ReadWithTimeout(10 * time.Second)
		require.True(t, ok)
		require.NotNil(t, b)
	}
	_, ok = channel.NewReader(t.Context(), s2.output).ReadWithTimeout(10 * time.Second)
	require.False(t, ok)
}

type streamTestData struct {
	output chan *common.Block
	input  chan *common.Envelope
	cancel context.CancelFunc
}

//nolint:gocognit // cognitive complexity 18 is questionable.
func startStream(t *testing.T, endpoint connection.Endpoint) *streamTestData {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	c, err := connection.NewSingleConnection(&connection.ClientConfig{Endpoint: &endpoint})
	require.NoError(t, err)
	client := ab.NewAtomicBroadcastClient(c)

	deliverStream, err := client.Deliver(ctx)
	require.NoError(t, err)
	seekEnv, seekErr := protoutil.CreateSignedEnvelope(
		common.HeaderType_DELIVER_SEEK_INFO, "", nil, &ab.SeekInfo{
			Start: &ab.SeekPosition{Type: &ab.SeekPosition_Specified{Specified: &ab.SeekSpecified{
				Number: 0,
			}}},
			Stop: &ab.SeekPosition{Type: &ab.SeekPosition_Specified{Specified: &ab.SeekSpecified{
				Number: 1_000,
			}}},
			Behavior: ab.SeekInfo_BLOCK_UNTIL_READY,
		}, 0, 0,
	)
	require.NoError(t, seekErr)
	err = deliverStream.Send(seekEnv)
	require.NoError(t, err)

	broadcastStream, err := client.Broadcast(ctx)
	require.NoError(t, err)

	wg := sync.WaitGroup{}
	t.Cleanup(wg.Wait)

	deliverQueue := make(chan *common.Block, 10)
	wg.Go(func() {
		output := channel.NewWriter(ctx, deliverQueue)
		for ctx.Err() == nil {
			res, deliverErr := deliverStream.Recv()
			if deliverErr != nil {
				return
			}
			output.Write(res.GetBlock())
		}
	})

	broadcastQueue := make(chan *common.Envelope, 10)
	wg.Go(func() {
		input := channel.NewReader(ctx, broadcastQueue)
		for ctx.Err() == nil {
			env, ok := input.Read()
			if !ok {
				return
			}
			broadcastErr := broadcastStream.Send(env)
			if broadcastErr != nil {
				return
			}
		}
	})
	wg.Go(func() {
		for ctx.Err() == nil {
			_, broadcastErr := broadcastStream.Recv()
			if broadcastErr != nil {
				return
			}
		}
	})

	return &streamTestData{
		output: deliverQueue,
		input:  broadcastQueue,
		cancel: cancel,
	}
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
