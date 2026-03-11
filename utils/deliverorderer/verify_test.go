/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package deliverorderer

import (
	"testing"

	"github.com/hyperledger/fabric-lib-go/common/flogging"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	commontypes "github.com/hyperledger/fabric-x-common/api/types"
	"github.com/hyperledger/fabric-x-common/protoutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/utils/deliver"
	"github.com/hyperledger/fabric-x-committer/utils/ordererconn"
	"github.com/hyperledger/fabric-x-committer/utils/testcrypto"
)

func BenchmarkVerifyBlock(b *testing.B) {
	flogging.Init(flogging.Config{LogSpec: "fatal"})
	printer := message.NewPrinter(language.English)
	for _, blockSize := range []int{100, 1_000, 10_000, 100_000} {
		b.Run(printer.Sprintf("blockSize=%d", blockSize), func(b *testing.B) {
			txs := workload.GenerateTransactions(b, nil, b.N)
			blocks := make([]*deliver.BlockWithSourceID, 0, 1+(b.N/blockSize))
			s, p := prepare(b)
			for len(txs) > 0 {
				curBlockSize := min(blockSize, len(txs))
				blk := workload.MapToOrdererBlock(0, txs[:curBlockSize])
				txs = txs[curBlockSize:]
				blocks = append(blocks, &deliver.BlockWithSourceID{Block: blk})
				testcrypto.PrepareBlockHeaderAndMetadata(blk, p)
				p.PrevBlock = blk
			}

			b.ResetTimer()
			for _, blk := range blocks {
				verErr := s.verificationStepAndUpdateState(blk)
				require.NoError(b, verErr)
			}
			b.StopTimer()
		})
	}
}

func TestVerifyBlock(t *testing.T) {
	t.Parallel()
	s, p := prepare(t)
	txs := workload.GenerateTransactions(t, nil, 100)
	b1 := workload.MapToOrdererBlock(1, txs)

	t.Log("[X] Well-formed block with no signatures")
	testcrypto.PrepareBlockHeaderAndMetadata(b1, testcrypto.BlockPrepareParameters{PrevBlock: p.PrevBlock})
	err := s.verificationStepAndUpdateState(&deliver.BlockWithSourceID{Block: b1})
	require.ErrorContains(t, err, "block signature verification failed on block")

	t.Log("[V] Well-formed block with signatures")
	testcrypto.PrepareBlockHeaderAndMetadata(b1, p)
	err = s.verificationStepAndUpdateState(&deliver.BlockWithSourceID{Block: b1, SourceID: 1})
	require.NoError(t, err)
	require.EqualValues(t, 1, s.updaterSourceID)

	t.Log("[X] Unexpected next block number")
	err = s.verificationStepAndUpdateState(&deliver.BlockWithSourceID{Block: b1})
	require.ErrorContains(t, err, "received [1] != [2] expected")

	t.Log("[X] Unexpected prev block hash")
	b1a := workload.MapToOrdererBlock(2, txs)
	testcrypto.PrepareBlockHeaderAndMetadata(b1a, testcrypto.BlockPrepareParameters{PrevBlock: p.PrevBlock})
	b1a.Header.Number = 2
	err = s.verificationStepAndUpdateState(&deliver.BlockWithSourceID{Block: b1a})
	require.ErrorContains(t, err, "previous block header hash mismatch")
	p.PrevBlock = b1

	// Test hash validation
	t.Log("[X] Block with data hash mismatch")
	b2 := workload.MapToOrdererBlock(1, txs)
	testcrypto.PrepareBlockHeaderAndMetadata(b2, p)
	b2.Header.DataHash = []byte("wrong hash")
	err = s.verificationStepAndUpdateState(&deliver.BlockWithSourceID{Block: b2})
	require.ErrorContains(t, err, "block data hash mismatch")

	t.Log("[V] New block")
	submitGoodBlock(t, &s, &p, 5)

	// Test policy validation
	t.Log("[V] New block with minimal quorum")
	minimalQuorum := p
	minimalQuorum.ConsenterSigners = minimalQuorum.ConsenterSigners[:3]
	submitGoodBlock(t, &s, &minimalQuorum, 6)
	p.PrevBlock = minimalQuorum.PrevBlock

	t.Log("[X] New block with insufficient quorum")
	insufficientQuorum := p
	insufficientQuorum.ConsenterSigners = insufficientQuorum.ConsenterSigners[:2]
	b4 := workload.MapToOrdererBlock(1, txs)
	testcrypto.PrepareBlockHeaderAndMetadata(b4, insufficientQuorum)
	err = s.verificationStepAndUpdateState(&deliver.BlockWithSourceID{Block: b4})
	require.ErrorContains(t, err, "2 sub-policies were satisfied, but this policy requires 3")

	t.Log("[V] Block with nil data in header-only stream")
	bHeaderOnly := workload.MapToOrdererBlock(1, txs)
	testcrypto.PrepareBlockHeaderAndMetadata(bHeaderOnly, p)
	bHeaderOnly.Data = nil // Make it header-only
	s.dataBlockStream = false
	err = s.verificationStepAndUpdateState(&deliver.BlockWithSourceID{Block: bHeaderOnly})
	require.NoError(t, err)
	s.dataBlockStream = true
	p.PrevBlock = bHeaderOnly

	// Test config block handling
	t.Log("[V] New Config block")
	_, p2 := prepare(t)
	newConfigBlock := p2.PrevBlock
	testcrypto.PrepareBlockHeaderAndMetadata(newConfigBlock, p)
	err = s.verificationStepAndUpdateState(&deliver.BlockWithSourceID{Block: newConfigBlock, SourceID: 2})
	require.NoError(t, err)
	require.EqualValues(t, 2, s.updaterSourceID)
	require.Equal(t, newConfigBlock.Header.Number, s.configBlockNumber)

	t.Log("[V] New block with new consenters")
	p2.LastConfigBlockIndex = p2.PrevBlock.Header.Number
	submitGoodBlock(t, &s, &p2, 2)

	t.Log("[X] Block points to older config block")
	p.PrevBlock = p2.PrevBlock
	b7 := workload.MapToOrdererBlock(1, txs)
	testcrypto.PrepareBlockHeaderAndMetadata(b7, p)
	err = s.verificationStepAndUpdateState(&deliver.BlockWithSourceID{Block: b7})
	require.Error(t, err, "expected error due to wrong consenters")
	require.ErrorContains(t, err, "block's last config block [0] != [5] current config block")

	t.Log("[X] Block with old consenters")
	p.LastConfigBlockIndex = p2.LastConfigBlockIndex
	testcrypto.PrepareBlockHeaderAndMetadata(b7, p)
	err = s.verificationStepAndUpdateState(&deliver.BlockWithSourceID{Block: b7})
	require.Error(t, err, "expected error due to wrong consenters")
	require.ErrorContains(t, err, "0 sub-policies were satisfied")
}

func TestVerifyBlockForm(t *testing.T) {
	t.Parallel()
	txs := workload.GenerateTransactions(t, nil, 100)

	for _, tc := range []struct {
		name          string
		block         *common.Block
		errorContains string
	}{
		{
			name:          "Nil block",
			block:         nil,
			errorContains: "malformed",
		},
		{
			name: "Block with nil header",
			block: &common.Block{
				Data:     &common.BlockData{},
				Metadata: &common.BlockMetadata{Metadata: make([][]byte, 5)},
			},
			errorContains: "malformed",
		},
		{
			name: "Block with nil metadata",
			block: &common.Block{
				Header: &common.BlockHeader{Number: 1},
				Data:   &common.BlockData{},
			},
			errorContains: "malformed",
		},
		{
			name: "Block with insufficient metadata length",
			block: &common.Block{
				Header:   &common.BlockHeader{Number: 1},
				Data:     &common.BlockData{},
				Metadata: &common.BlockMetadata{Metadata: make([][]byte, 2)},
			},
			errorContains: "malformed",
		},
		{
			name: "Block with nil data in data block stream",
			block: &common.Block{
				Header:   &common.BlockHeader{Number: 1},
				Metadata: &common.BlockMetadata{Metadata: make([][]byte, 5)},
			},
			errorContains: "malformed",
		},
		{
			name:          "Malformed block",
			block:         workload.MapToOrdererBlock(1, txs),
			errorContains: "malformed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, _ := prepare(t)
			err := s.verificationStepAndUpdateState(&deliver.BlockWithSourceID{Block: tc.block})
			require.Error(t, err)
			require.ErrorContains(t, err, tc.errorContains)
		})
	}
}

func TestVerifyBlockEdgeCases(t *testing.T) {
	t.Parallel()
	txs := workload.GenerateTransactions(t, nil, 100)

	t.Run("Block number less than config block", func(t *testing.T) {
		t.Parallel()
		s, p := prepare(t)
		// Advance to block 5 first
		for range 5 {
			bTemp := workload.MapToOrdererBlock(0, txs)
			testcrypto.PrepareBlockHeaderAndMetadata(bTemp, p)
			err := s.verificationStepAndUpdateState(&deliver.BlockWithSourceID{Block: bTemp})
			require.NoError(t, err)
			p.PrevBlock = bTemp
		}
		// Now manually set config block to 10 (simulating we received a config block externally)
		s.configBlockNumber = 10
		// Try to process block 6, which is less than config block 10
		b := workload.MapToOrdererBlock(0, txs)
		p.LastConfigBlockIndex = 10
		testcrypto.PrepareBlockHeaderAndMetadata(b, p)
		err := s.verificationStepAndUpdateState(&deliver.BlockWithSourceID{Block: b})
		require.Error(t, err)
		require.ErrorContains(t, err, "block number [6] is less than config block number [10]")
	})

	t.Run("Genesis block mismatch", func(t *testing.T) {
		t.Parallel()
		// Create a state with one genesis block loaded
		cryptoDir := t.TempDir()
		genesisBlock, err := testcrypto.CreateOrExtendConfigBlockWithCrypto(cryptoDir, &testcrypto.ConfigBlock{
			ChannelID: "my-chan",
			OrdererEndpoints: []*commontypes.OrdererEndpoint{
				{ID: 0, Host: "127.0.0.1", Port: 1000},
			},
		})
		require.NoError(t, err)
		s := newBlockProcessingState(true)
		err = s.updateIfConfigBlock(genesisBlock)
		require.NoError(t, err)
		// Don't process it yet, just set it as config

		// Now try to verify a different genesis block (block 0)
		cryptoDir2 := t.TempDir()
		differentGenesis, err := testcrypto.CreateOrExtendConfigBlockWithCrypto(cryptoDir2, &testcrypto.ConfigBlock{
			ChannelID: "my-chan",
			OrdererEndpoints: []*commontypes.OrdererEndpoint{
				{ID: 0, Host: "127.0.0.1", Port: 2000}, // Different port
			},
		})
		require.NoError(t, err)
		err = s.verificationStepAndUpdateState(&deliver.BlockWithSourceID{Block: differentGenesis})
		require.Error(t, err)
		require.ErrorContains(t, err, "delivered genesis block mismatch")
	})

	// Test config block with channel ID mismatch
	t.Run("Config block with channel ID mismatch", func(t *testing.T) {
		t.Parallel()
		cryptoDir := t.TempDir()
		genesisBlock, err := testcrypto.CreateOrExtendConfigBlockWithCrypto(cryptoDir, &testcrypto.ConfigBlock{
			ChannelID: "channel-1",
			OrdererEndpoints: []*commontypes.OrdererEndpoint{
				{ID: 0, Host: "127.0.0.1", Port: 1000},
			},
		})
		require.NoError(t, err)

		s := newBlockProcessingState(false)
		err = s.updateIfConfigBlock(genesisBlock)
		require.NoError(t, err)

		// Now try to update with a config block from a different channel
		cryptoDir2 := t.TempDir()
		wrongChanBlock, err := testcrypto.CreateOrExtendConfigBlockWithCrypto(cryptoDir2, &testcrypto.ConfigBlock{
			ChannelID: "channel-2", // Different channel
			OrdererEndpoints: []*commontypes.OrdererEndpoint{
				{ID: 0, Host: "127.0.0.1", Port: 1000},
			},
		})
		require.NoError(t, err)

		err = s.updateIfConfigBlock(wrongChanBlock)
		require.Error(t, err)
		require.ErrorContains(t, err, "config block channel ID [channel-2] does not match expected [channel-1]")
	})

	// Test non-config block with wrong last config index
	t.Run("Non-config block pointing to wrong config block", func(t *testing.T) {
		t.Parallel()
		s, p := prepare(t)
		// Advance to block 4
		for range 4 {
			bTemp := workload.MapToOrdererBlock(0, txs)
			testcrypto.PrepareBlockHeaderAndMetadata(bTemp, p)
			err := s.verificationStepAndUpdateState(&deliver.BlockWithSourceID{Block: bTemp})
			require.NoError(t, err)
			p.PrevBlock = bTemp
		}
		// Create a regular (non-config) block at position 5 but make it point to wrong config block
		badBlock := workload.MapToOrdererBlock(0, txs)
		testcrypto.PrepareBlockHeaderAndMetadata(badBlock, testcrypto.BlockPrepareParameters{
			LastConfigBlockIndex: 3, // Should point to 0 (current config) but points to 3
			PrevBlock:            p.PrevBlock,
			ConsenterSigners:     p.ConsenterSigners,
		})
		err := s.verificationStepAndUpdateState(&deliver.BlockWithSourceID{Block: badBlock})
		require.Error(t, err)
		require.ErrorContains(t, err, "block's last config block [3] != [0] current config block")
	})

	t.Run("Valid config block envelope with invalid config block", func(t *testing.T) {
		t.Parallel()
		s, p := prepare(t)
		// Advance to block 1
		bTemp := workload.MapToOrdererBlock(0, txs)
		testcrypto.PrepareBlockHeaderAndMetadata(bTemp, p)
		err := s.verificationStepAndUpdateState(&deliver.BlockWithSourceID{Block: bTemp})
		require.NoError(t, err)
		p.PrevBlock = bTemp

		// Create a config block with invalid config data at block 2
		invalidConfigBlock2 := &common.Block{
			Header: &common.BlockHeader{Number: 2, PreviousHash: s.prevBlockHeaderHash},
			Data: &common.BlockData{
				Data: [][]byte{
					protoutil.MarshalOrPanic(&common.Envelope{
						Payload: protoutil.MarshalOrPanic(&common.Payload{
							Header: &common.Header{
								ChannelHeader: protoutil.MarshalOrPanic(&common.ChannelHeader{
									Type:      int32(common.HeaderType_CONFIG),
									ChannelId: "my-chan",
								}),
							},
							Data: []byte("invalid config envelope data"),
						}),
					}),
				},
			},
			Metadata: &common.BlockMetadata{Metadata: make([][]byte, 5)},
		}
		testcrypto.PrepareBlockHeaderAndMetadata(invalidConfigBlock2, p)
		// This should succeed in verification but log a warning about config update failure (line 89)
		err = s.verificationStepAndUpdateState(&deliver.BlockWithSourceID{Block: invalidConfigBlock2})
		require.NoError(t, err, "block should be delivered even if config update fails")
		assert.EqualValues(t, 3, s.nextBlockNum, "state should be updated despite config error")
	})

	t.Run("No verifierFunc", func(t *testing.T) {
		t.Parallel()
		s, p := prepare(t)
		s.configState = configState{
			verifierFunc:        nil, // No verifier function
			ConfigBlockMaterial: nil, // No config block
		}
		p.ConsenterSigners = nil
		// Create a simple block without verification
		simpleBlock := workload.MapToOrdererBlock(1, txs)
		simpleBlock.Header.Number = 0
		simpleBlock.Metadata = &common.BlockMetadata{Metadata: make([][]byte, 5)}
		testcrypto.PrepareBlockHeaderAndMetadata(simpleBlock, p)
		err := s.verificationStepAndUpdateState(&deliver.BlockWithSourceID{Block: simpleBlock})
		require.NoError(t, err, "should succeed when verifierFunc is nil")
	})

	t.Run("Config block that does not reference itself", func(t *testing.T) {
		t.Parallel()
		s, p := prepare(t)
		// Create a config block at position 5 that points to block 3 instead of itself
		cryptoDir := t.TempDir()
		badConfigBlock, err := testcrypto.CreateOrExtendConfigBlockWithCrypto(cryptoDir, &testcrypto.ConfigBlock{
			ChannelID: s.ChannelID,
			OrdererEndpoints: []*commontypes.OrdererEndpoint{
				{ID: 0, Host: "127.0.0.1", Port: 1000},
			},
		})
		require.NoError(t, err)
		testcrypto.PrepareBlockHeaderAndMetadata(badConfigBlock, p)
		m := &common.Metadata{}
		err = proto.Unmarshal(badConfigBlock.Metadata.Metadata[common.BlockMetadataIndex_SIGNATURES], m)
		require.NoError(t, err)
		ordererMetadata := &common.OrdererBlockMetadata{LastConfig: &common.LastConfig{Index: 3}}
		m.Value = protoutil.MarshalOrPanic(ordererMetadata)
		badConfigBlock.Metadata.Metadata[common.BlockMetadataIndex_SIGNATURES] = protoutil.MarshalOrPanic(m)

		err = s.verificationStepAndUpdateState(&deliver.BlockWithSourceID{Block: badConfigBlock})
		require.Error(t, err)
		require.ErrorContains(t, err, "config block's last config block [3] != [1] config block number")
	})
}

func TestUpdateIfConfigBlock(t *testing.T) {
	t.Parallel()

	t.Run("Non-config block", func(t *testing.T) {
		t.Parallel()
		cs := configState{}
		txs := workload.GenerateTransactions(t, nil, 100)
		b := workload.MapToOrdererBlock(1, txs)

		err := cs.updateIfConfigBlock(b)
		require.ErrorIs(t, err, ordererconn.ErrNotConfigBlock)
		require.Nil(t, cs.ConfigBlockMaterial)
	})

	t.Run("Channel ID mismatch", func(t *testing.T) {
		t.Parallel()
		cs := configState{}
		cryptoDir := t.TempDir()
		configBlock, err := testcrypto.CreateOrExtendConfigBlockWithCrypto(cryptoDir, &testcrypto.ConfigBlock{
			ChannelID: "existing-channel",
			OrdererEndpoints: []*commontypes.OrdererEndpoint{
				{ID: 0, Host: "127.0.0.1", Port: 1000},
			},
		})
		require.NoError(t, err)
		err = cs.updateIfConfigBlock(configBlock)
		require.NoError(t, err)

		cryptoDir2 := t.TempDir()
		configBlock2, err := testcrypto.CreateOrExtendConfigBlockWithCrypto(cryptoDir2, &testcrypto.ConfigBlock{
			ChannelID: "different-channel",
			OrdererEndpoints: []*commontypes.OrdererEndpoint{
				{ID: 0, Host: "127.0.0.1", Port: 1000},
			},
		})
		require.NoError(t, err)

		err = cs.updateIfConfigBlock(configBlock2)
		require.ErrorContains(t, err,
			"config block channel ID [different-channel] does not match expected [existing-channel]")
	})

	t.Run("Valid config block updates state", func(t *testing.T) {
		t.Parallel()
		cs := configState{}
		cryptoDir := t.TempDir()
		configBlock, err := testcrypto.CreateOrExtendConfigBlockWithCrypto(cryptoDir, &testcrypto.ConfigBlock{
			ChannelID: "test-channel",
			OrdererEndpoints: []*commontypes.OrdererEndpoint{
				{ID: 0, Host: "127.0.0.1", Port: 1000},
			},
		})
		require.NoError(t, err)

		err = cs.updateIfConfigBlock(configBlock)
		require.NoError(t, err)
		assert.Equal(t, "test-channel", cs.ChannelID)
		assert.Equal(t, configBlock.Header.Number, cs.configBlockNumber)
		assert.NotNil(t, cs.ConfigBlock)
		assert.NotNil(t, cs.verifierFunc)
		assert.Len(t, cs.OrdererOrganizations, 1)
	})

	t.Run("Malformed config block (invalid envelope) - treated as non-config", func(t *testing.T) {
		t.Parallel()
		cs := configState{}
		malformedBlock := &common.Block{
			Header: &common.BlockHeader{Number: 0},
			Data: &common.BlockData{
				Data: [][]byte{[]byte("invalid envelope data")},
			},
		}
		err := cs.updateIfConfigBlock(malformedBlock)
		// malformed blocks that can't be parsed are treated as non-config blocks
		require.ErrorIs(t, err, ordererconn.ErrNotConfigBlock)
		assert.Nil(t, cs.ConfigBlockMaterial, "config state should not be updated for malformed blocks")
	})

	t.Run("Valid config block envelope with invalid config block", func(t *testing.T) {
		t.Parallel()
		cs := configState{}
		// Create a block that looks like a config block (single transaction) but has invalid config data
		invalidConfigBlock := &common.Block{
			Header: &common.BlockHeader{Number: 0},
			Data: &common.BlockData{
				Data: [][]byte{
					protoutil.MarshalOrPanic(&common.Envelope{
						Payload: protoutil.MarshalOrPanic(&common.Payload{
							Header: &common.Header{
								ChannelHeader: protoutil.MarshalOrPanic(&common.ChannelHeader{
									Type:      int32(common.HeaderType_CONFIG),
									ChannelId: "test-channel",
								}),
							},
							Data: []byte("invalid config envelope data"),
						}),
					}),
				},
			},
		}
		err := cs.updateIfConfigBlock(invalidConfigBlock)
		require.ErrorContains(t, err, "error unmarshalling config envelope")
		assert.Nil(t, cs.ConfigBlockMaterial, "config state should not be updated for invalid config blocks")
	})
}

func prepare(tb testing.TB) (blockVerificationStateMachine, testcrypto.BlockPrepareParameters) {
	tb.Helper()
	cryptoDir := tb.TempDir()
	genesisBlock, err := testcrypto.CreateOrExtendConfigBlockWithCrypto(cryptoDir, &testcrypto.ConfigBlock{
		ChannelID: "my-chan",
		OrdererEndpoints: []*commontypes.OrdererEndpoint{
			{ID: 0, Host: "127.0.0.1", Port: 1000},
			{ID: 1, Host: "127.0.0.1", Port: 1001},
			{ID: 2, Host: "127.0.0.1", Port: 1002},
			{ID: 3, Host: "127.0.0.1", Port: 1003},
			{ID: 4, Host: "127.0.0.1", Port: 1004},
		},
	})
	require.NoError(tb, err)

	s := newBlockProcessingState(true)
	// We process the genesis block first.
	err = s.verificationStepAndUpdateState(&deliver.BlockWithSourceID{Block: genesisBlock})
	require.NoError(tb, err)

	identities, idErr := testcrypto.GetConsenterIdentities(cryptoDir)
	require.NoError(tb, idErr)
	return s, testcrypto.BlockPrepareParameters{
		LastConfigBlockIndex: 0,
		PrevBlock:            genesisBlock,
		ConsenterSigners:     identities,
	}
}

func submitGoodBlock(
	tb testing.TB, s *blockVerificationStateMachine, p *testcrypto.BlockPrepareParameters, label uint32,
) {
	tb.Helper()
	txs := workload.GenerateTransactions(tb, nil, 100)
	b := workload.MapToOrdererBlock(1, txs)
	testcrypto.PrepareBlockHeaderAndMetadata(b, *p)
	err := s.verificationStepAndUpdateState(&deliver.BlockWithSourceID{Block: b, SourceID: label})
	require.NoError(tb, err)
	require.Equal(tb, label, s.updaterSourceID)
	p.PrevBlock = b
}
