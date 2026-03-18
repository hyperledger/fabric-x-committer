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
	"github.com/hyperledger/fabric-x-common/api/types"
	"github.com/hyperledger/fabric-x-common/common/policies"
	"github.com/hyperledger/fabric-x-common/protoutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/ordererconn"
	"github.com/hyperledger/fabric-x-committer/utils/test"
	"github.com/hyperledger/fabric-x-committer/utils/testcrypto"
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

func TestDefaultGenesisBlock(t *testing.T) {
	t.Parallel()
	require.True(t, protoutil.IsConfigBlock(defaultConfigBlock))
}

type blockFetcher struct {
	expectedBlock uint64
	verifier      protoutil.BlockVerifierFunc
}

// TestOrderer tests just the testing functionality.
// It does not validate the streaming API.
func TestOrderer(t *testing.T) {
	t.Parallel()
	artifactsPath := t.TempDir()
	genesisBlock, err := testcrypto.CreateOrExtendConfigBlockWithCrypto(artifactsPath, &testcrypto.ConfigBlock{
		ChannelID:             "test-channel",
		OrdererEndpoints:      []*types.OrdererEndpoint{{ID: 0, Host: "localhost", Port: 7050}},
		PeerOrganizationCount: 1,
	})
	require.NoError(t, err)
	f := &blockFetcher{
		expectedBlock: 0,
		verifier:      getVerifier(t, genesisBlock),
	}

	o, err := NewMockOrderer(&OrdererConfig{
		BlockSize:        3,
		BlockTimeout:     time.Hour,
		OutBlockCapacity: 10,
		PayloadCacheSize: 10,
		SendGenesisBlock: true,
		ArtifactsPath:    artifactsPath,
	})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	t.Cleanup(cancel)
	test.RunServiceForTest(ctx, t, func(ctx context.Context) error {
		return connection.FilterStreamRPCError(o.Run(ctx))
	}, o.WaitForReady)

	t.Log("Config block")
	block, _ := f.requireGetBlock(ctx, t, o, 1)
	test.RequireProtoEqual(t, genesisBlock, block)

	t.Log("Get wait and fail")
	waitCtx, waitCancel := context.WithTimeout(ctx, 3*time.Second)
	t.Cleanup(waitCancel)
	block, err = o.GetBlock(waitCtx, f.expectedBlock)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Nil(t, block)

	t.Log("Insert block and get it")
	sentBlock := makeBlockData(f.expectedBlock)
	require.NoError(t, o.SubmitBlock(ctx, sentBlock))
	block, _ = f.requireGetBlock(ctx, t, o, 1)
	require.Equal(t, sentBlock.Data.Data, block.Data.Data)

	t.Log("Submit env and force cut")
	for i := range 3 {
		sentEnv := makeEnvelopePayload(i + 1)
		require.True(t, o.SubmitEnv(ctx, sentEnv))
		require.True(t, o.CutBlock(ctx))
		_, e := f.requireGetBlock(ctx, t, o, 1)
		require.Equal(t, sentEnv.Payload, e[0].Payload)
	}

	t.Log("Size cut")
	ctx, cancel = context.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(cancel)
	for i := range 3 {
		require.True(t, o.SubmitEnv(ctx, makeEnvelopePayload(i+10)))
	}

	_, envs := f.requireGetBlock(ctx, t, o, 3)
	for i, e := range envs {
		require.Equal(t, makeEnvelopePayload(i+10).Payload, e.Payload)
	}

	t.Log("Timeout cut")
	require.True(t, o.SubmitEnv(ctx, makeEnvelopePayload(20)))

	waitCtx, waitCancel = context.WithTimeout(ctx, 3*time.Second)
	t.Cleanup(waitCancel)
	block, err = o.GetBlock(waitCtx, f.expectedBlock)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Nil(t, block)

	// We manually override this config, so it will be used after the next cut.
	o.config.BlockTimeout = time.Second

	require.True(t, o.CutBlock(ctx))
	// We ignore one block.
	f.expectedBlock++

	require.True(t, o.SubmitEnv(ctx, makeEnvelopePayload(30)))

	_, e := f.requireGetBlock(ctx, t, o, 1)
	require.Equal(t, makeEnvelopePayload(30).Payload, e[0].Payload)

	t.Log("New config block")
	newConfigBlock, err := testcrypto.CreateOrExtendConfigBlockWithCrypto(artifactsPath, &testcrypto.ConfigBlock{
		ChannelID: "test-channel",
		OrdererEndpoints: []*types.OrdererEndpoint{
			{ID: 0, Host: "localhost", Port: 7050},
			{ID: 1, Host: "localhost", Port: 7050},
			{ID: 2, Host: "localhost", Port: 7050},
		},
		PeerOrganizationCount: 1,
	})
	require.NoError(t, err)
	consenters, err := testcrypto.GetConsenterIdentities(artifactsPath)
	require.NoError(t, err)
	err = o.SubmitBlockWithConsenters(ctx, &BlockWithConsenters{
		Block:            newConfigBlock,
		ConsenterSigners: consenters,
	})
	require.NoError(t, err)

	block, _ = f.requireGetBlock(ctx, t, o, 1)
	test.RequireProtoEqual(t, newConfigBlock.Data, block.Data)

	t.Log("Submit block with new config and verify it is verified with the new config")
	f.verifier = getVerifier(t, newConfigBlock)
	require.NoError(t, o.SubmitBlock(ctx, makeBlockData(f.expectedBlock)))
	block, _ = f.requireGetBlock(ctx, t, o, 1)
	lastConfigIndex, err := protoutil.GetLastConfigIndexFromBlock(block)
	require.NoError(t, err)
	require.EqualValues(t, 8, lastConfigIndex)
}

func (f *blockFetcher) requireGetBlock(
	ctx context.Context, t *testing.T, o *Orderer, expectedSize int,
) (*common.Block, []*common.Envelope) {
	t.Helper()
	block, err := o.GetBlock(ctx, f.expectedBlock)
	require.NoError(t, err)
	require.NotNil(t, block)
	require.Equal(t, f.expectedBlock, block.Header.Number)
	require.Len(t, block.Data.Data, expectedSize)
	f.expectedBlock++

	if block.Header.Number > 0 {
		require.NoError(t, f.verifier(block.Header, block.Metadata))
	}

	ret := make([]*common.Envelope, expectedSize)
	for i, data := range block.Data.Data {
		e, uErr := protoutil.UnmarshalEnvelope(data)
		require.NoError(t, uErr)
		ret[i] = e
	}
	return block, ret
}

func getVerifier(t *testing.T, block *common.Block) protoutil.BlockVerifierFunc {
	t.Helper()
	configBlock, err := ordererconn.LoadConfigBlock(block)
	require.NoError(t, err)
	require.NotNil(t, configBlock)

	policy, exists := configBlock.Bundle.PolicyManager().GetPolicy(policies.BlockValidation)
	require.True(t, exists)

	oc, ok := configBlock.Bundle.OrdererConfig()
	require.True(t, ok)

	bftEnabled := configBlock.Bundle.ChannelConfig().Capabilities().ConsensusTypeBFT()
	require.True(t, bftEnabled)
	consenters := oc.Consenters()

	return protoutil.BlockSignatureVerifier(bftEnabled, consenters, policy)
}

// TestOrdererStreamingAPI tests the streaming API and the stream state registry.
func TestOrdererStreamingAPI(t *testing.T) {
	t.Parallel()
	o, err := NewMockOrderer(&OrdererConfig{
		BlockSize:        1,
		SendGenesisBlock: true,
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
	s0 := startStream(t, ep0, ab.SeekInfo_BLOCK)
	RequireStreams(t, o, 1)
	RequireStreamsWithEndpoints(t, o, 1, addr0)

	s1 := startStream(t, ep1, ab.SeekInfo_BLOCK)
	RequireStreams(t, o, 2)
	RequireStreamsWithEndpoints(t, o, 1, addr1)

	s2 := startStream(t, ep2, ab.SeekInfo_BLOCK)
	RequireStreams(t, o, 3)
	RequireStreamsWithEndpoints(t, o, 1, addr2)

	t.Log("Open a second stream for endpoint")
	s3 := startStream(t, ep2, ab.SeekInfo_BLOCK)
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

	s0 = startStream(t, ep0, ab.SeekInfo_BLOCK)
	streamState0 := RequireStreamsWithEndpoints(t, o, 1, addr0)
	require.Len(t, streamState0, 1)
	require.EqualValues(t, 1, streamState0[0].PartyID)
	require.True(t, streamState0[0].DataBlockStream)

	s1 = startStream(t, ep1, ab.SeekInfo_BLOCK)
	streamState1 := RequireStreamsWithEndpoints(t, o, 1, addr1)
	require.Len(t, streamState1, 1)
	require.EqualValues(t, 1, streamState1[0].PartyID)
	require.True(t, streamState1[0].DataBlockStream)

	s2 = startStream(t, ep2, ab.SeekInfo_BLOCK)
	streamState2 := RequireStreamsWithEndpoints(t, o, 1, addr2)
	require.Len(t, streamState2, 1)
	require.EqualValues(t, 2, streamState2[0].PartyID)
	require.True(t, streamState2[0].DataBlockStream)

	t.Log("Get genesis block")
	for _, stream := range []*streamTestData{s0, s1, s2} {
		b, ok := channel.NewReader(t.Context(), stream.output).ReadWithTimeout(10 * time.Second)
		require.True(t, ok)
		require.NotNil(t, b)
		require.EqualValues(t, 0, b.Header.Number)
	}

	t.Log("Send and receive block (sanity check)")
	channel.NewWriter(t.Context(), s0.input).Write(&common.Envelope{Payload: []byte{1}})
	for _, stream := range []*streamTestData{s0, s1, s2} {
		b, ok := channel.NewReader(t.Context(), stream.output).ReadWithTimeout(10 * time.Second)
		require.True(t, ok)
		require.NotNil(t, b)
		require.EqualValues(t, 1, b.Header.Number)
	}

	t.Log("Hold party 1")
	p1.HoldFromBlock.Store(2)
	channel.NewWriter(t.Context(), s1.input).Write(&common.Envelope{Payload: []byte{2}})
	b, ok := channel.NewReader(t.Context(), s2.output).ReadWithTimeout(10 * time.Second)
	require.True(t, ok)
	require.NotNil(t, b)
	require.EqualValues(t, 2, b.Header.Number)
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
		require.EqualValues(t, 2, b.Header.Number)
	}

	p2.HoldFromBlock.Store(3)
	channel.NewWriter(t.Context(), s2.input).Write(&common.Envelope{Payload: []byte{3}})
	for _, stream := range []*streamTestData{s0, s1} {
		b, ok = channel.NewReader(t.Context(), stream.output).ReadWithTimeout(10 * time.Second)
		require.True(t, ok)
		require.NotNil(t, b)
		require.EqualValues(t, 3, b.Header.Number)
	}
	_, ok = channel.NewReader(t.Context(), s2.output).ReadWithTimeout(10 * time.Second)
	require.False(t, ok)

	t.Log("Replace party 2 block and release")
	fakeBlock := makeBlockData(10)
	fakeBlock.Header = &common.BlockHeader{Number: 195, DataHash: []byte{1, 2, 3}}
	p2.ReplaceBlock.Store(3, fakeBlock)
	p2.HoldFromBlock.Store(4)
	b, ok = channel.NewReader(t.Context(), s2.output).ReadWithTimeout(10 * time.Second)
	require.True(t, ok)
	require.NotNil(t, b)
	test.RequireProtoEqual(t, fakeBlock, b)

	t.Log("Headers only stream")
	s2.cancel()
	RequireStreamsWithEndpoints(t, o, 0, addr2)

	s2 = startStream(t, ep2, ab.SeekInfo_HEADER_WITH_SIG)
	streamState2 = RequireStreamsWithEndpoints(t, o, 1, addr2)
	require.Len(t, streamState2, 1)
	require.EqualValues(t, 2, streamState2[0].PartyID)
	require.False(t, streamState2[0].DataBlockStream)

	t.Log("Config block should arrive with the data")
	b, ok = channel.NewReader(t.Context(), s2.output).ReadWithTimeout(10 * time.Second)
	require.True(t, ok)
	require.NotNil(t, b)
	require.NotNil(t, b.Header)
	require.EqualValues(t, 0, b.Header.Number)
	require.NotNil(t, b.Data)
	require.NotEmpty(t, b.Data.Data)

	t.Log("Data block should arrive with only header")
	for i := range 2 {
		b, ok = channel.NewReader(t.Context(), s2.output).ReadWithTimeout(10 * time.Second)
		require.True(t, ok)
		require.NotNil(t, b)
		require.NotNil(t, b.Header)
		require.EqualValues(t, i+1, b.Header.Number)
		require.Nil(t, b.Data)
	}

	t.Log("Receive the fake block with only header and verify the correct header")
	b, ok = channel.NewReader(t.Context(), s2.output).ReadWithTimeout(10 * time.Second)
	require.True(t, ok)
	require.NotNil(t, b)
	require.Nil(t, b.Data)
	require.NotNil(t, b.Header)
	test.RequireProtoEqual(t, fakeBlock.Header, b.Header)
}

type streamTestData struct {
	output chan *common.Block
	input  chan *common.Envelope
	cancel context.CancelFunc
}

//nolint:gocognit // cognitive complexity 18 is questionable.
func startStream(t *testing.T, endpoint connection.Endpoint, contentType ab.SeekInfo_SeekContentType) *streamTestData {
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
			Start:       seekPosition(0),
			Stop:        seekPosition(1_000),
			Behavior:    ab.SeekInfo_BLOCK_UNTIL_READY,
			ContentType: contentType,
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

func seekPosition(i uint64) *ab.SeekPosition {
	return &ab.SeekPosition{Type: &ab.SeekPosition_Specified{Specified: &ab.SeekSpecified{
		Number: i,
	}}}
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
		Payload:   fmt.Appendf(nil, "%d", i),
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
			Data: [][]byte{protoutil.MarshalOrPanic(makeEnvelopePayload(int(i)))}, //nolint:gosec // int -> uint64.
		},
	}
}

func makeEnvelopePayload(i int) *common.Envelope {
	return &common.Envelope{
		Payload: fmt.Appendf(nil, "%d", i),
	}
}
