/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package deliverorderer

import (
	"bytes"
	"math"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-x-common/common/channelconfig"
	"github.com/hyperledger/fabric-x-common/common/policies"
	"github.com/hyperledger/fabric-x-common/protoutil"
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-committer/utils/deliver"
)

type (
	// blockVerificationStateMachine maintains the state required for processing and verifying blocks
	// in sequence. It tracks the expected next block number, the last processed block,
	// and configuration for content verification.
	blockVerificationStateMachine struct {
		configState
		dataBlockStream     bool
		nextBlockNum        uint64
		lastBlockHeaderHash []byte
		lastBlock           *common.Block
		// updaterSourceID is used to track which source the last block was received from.
		updaterSourceID uint32
	}

	// configState holds the current channel configuration state used for block verification.
	// It contains the latest config block material.
	configState struct {
		*channelconfig.ConfigBlockMaterial
		configBlockNumber uint64
		verifierFunc      protoutil.BlockVerifierFunc
	}
)

// ErrUnexpectedBlockNumber is returned by the verification step if the blocks are not received in order.
var ErrUnexpectedBlockNumber = errors.New("received unexpected block number")

// newBlockProcessingState initializes the processing state from the last block
// and start the following blocks processing from it.
// It returns a headers only stream state, and the latest config state.
// For data block verification, use [state.cloneAsDataBlockStream()].
func newBlockProcessingState(session *SessionInfo) (
	state blockVerificationStateMachine, latestConfig configState, err error,
) {
	// We use headers-only stream to allow providing data-less block as the last block.
	// We process the last block before applying the config block to avoid verifying the last block.
	// This is because the last block might be signed by previous configuration.
	blockHeader := session.LastBlock.GetHeader()
	if blockHeader != nil {
		state.nextBlockNum = blockHeader.Number
		err = state.verificationStepAndUpdateState(&deliver.BlockWithSourceID{
			Block: session.LastBlock,
			// We use a large ID to ensure we do not confuse it with a real source.
			SourceID: math.MaxUint32,
		})
		if err != nil {
			return state, latestConfig, errors.WithMessage(err, "error loading last block")
		}
	}

	if session.NextBlockVerificationConfig != nil {
		err = state.updateIfConfigBlock(session.NextBlockVerificationConfig)
		if err != nil {
			return state, latestConfig, errors.WithMessage(err, "error loading next block verification config")
		}
	}

	if session.LatestKnownConfig != nil {
		err = latestConfig.updateIfConfigBlock(session.LatestKnownConfig)
		if err != nil {
			return state, latestConfig, errors.WithMessage(err, "error loading last known config")
		}
	}

	// We assert that the latest config is indeed the latest.
	// This is useful in cases that the latest config block was given by the user,
	// but the system have a newer config block in the ledger.
	if latestConfig.ConfigBlockMaterial == nil || state.configBlockNumber > latestConfig.configBlockNumber {
		latestConfig = state.configState
	}

	// We validate that the config block is ahead the next expected block to fail fast.
	if state.nextBlockNum < state.configBlockNumber {
		return state, latestConfig, errors.Newf(
			"verification config block number [%d] is ahead of the next expected block [%d]",
			state.configBlockNumber, state.nextBlockNum,
		)
	}
	return state, latestConfig, nil
}

// cloneAsDataBlockStream returns a clone of this state for data block verification.
func (s *blockVerificationStateMachine) cloneAsDataBlockStream() blockVerificationStateMachine {
	dataBlockStream := *s
	dataBlockStream.dataBlockStream = true
	return dataBlockStream
}

// verificationStepAndUpdateState returns error if the block number is not what expected,
// or if the blocks header/data hashes does not match the expected value.
func (s *blockVerificationStateMachine) verificationStepAndUpdateState(blk *deliver.BlockWithSourceID) error {
	// 1. Verify form.
	if !s.verifyBlockForm(blk.Block) {
		return errors.New("received malformed block")
	}

	// 2. Verify correct progress.
	if blk.Block.Header.Number != s.nextBlockNum {
		return errors.Wrapf(ErrUnexpectedBlockNumber, "received [%d] != [%d] expected",
			blk.Block.Header.Number, s.nextBlockNum)
	}

	// 3. Verify content.
	if err := s.verifyHashes(blk.Block); err != nil {
		return err
	}
	if err := s.verifyBlockPolicy(blk.Block); err != nil {
		return err
	}

	// If it is a valid block, update internal processing state.
	s.nextBlockNum = blk.Block.Header.Number + 1
	s.lastBlock = blk.Block
	s.lastBlockHeaderHash = protoutil.BlockHeaderHash(blk.Block.Header)
	s.updaterSourceID = blk.SourceID

	// If it is a config block, update the config state.
	err := s.updateIfConfigBlock(blk.Block)
	if err != nil && !errors.Is(err, channelconfig.ErrNotConfigBlock) {
		// At this point, the block is valid and should be delivered.
		// We can only log the issue for investigative purposes.
		logger.Warnf("failed to update config block: %v", err)
	}
	return nil
}

// verifyBlockForm checks whether the block is well-formed.
// It is only used internally.
func (s *blockVerificationStateMachine) verifyBlockForm(block *common.Block) bool {
	if block == nil || block.Header == nil || block.Metadata == nil {
		return false
	}
	if len(block.Metadata.Metadata) < len(common.BlockMetadataIndex_name) {
		return false
	}
	// Block data can be nil only when headers alone were requested.
	return !s.dataBlockStream || block.Data != nil
}

// verifyHashes checks that the block previous hash matches the last block,
// and that the internal data hash matches the one in the header.
// It is only used internally.
func (s *blockVerificationStateMachine) verifyHashes(block *common.Block) error {
	blockNumber := block.Header.Number

	// Verify header hash if we have the previous block hash.
	if len(s.lastBlockHeaderHash) != 0 {
		if !bytes.Equal(block.Header.PreviousHash, s.lastBlockHeaderHash) {
			return errors.Newf("previous block header hash mismatch on block [%d]", blockNumber)
		}
	}

	// Block's data can be nil only if we only requested headers.
	// However, the block's data can be non-nil even if we only requested headers in case of config block updates.
	// Thus, we always verify the data hash if data is present.
	if block.Data != nil {
		dataHash := protoutil.ComputeBlockDataHash(block.Data)
		// Verify that Header.DataHash is equal to the hash of block.Data
		// This is to ensure that the header is consistent with the data carried by this block
		if !bytes.Equal(dataHash, block.Header.DataHash) {
			return errors.Newf("block data hash mismatch on block [%d]", blockNumber)
		}
	}
	return nil
}

// verifyBlockPolicy returns error if the block does not satisfy the config block policy.
// It is only used internally.
func (s *blockVerificationStateMachine) verifyBlockPolicy(block *common.Block) error {
	if s.ConfigBlockMaterial == nil || s.verifierFunc == nil {
		return nil
	}

	blockNumber := block.Header.Number

	// If the config block is ahead of this block, something wrong has happened.
	// This should never happen as we validate in-order block processing.
	// But if we received a config block from external source via the input parameters, such error may occur.
	// For example, if the last known config was given from a corrupted ledger, or a male configured YAML file.
	if blockNumber < s.configBlockNumber {
		return errors.Newf("block number [%d] is less than config block number [%d]",
			blockNumber, s.configBlockNumber)
	}

	// We verify that the block points to the correct config block before we verify it.
	// Fabric's orderer verify that the block actually points to the correct config block.
	// so we expect it to be correct.
	lastConfigBlockIdx, err := protoutil.GetLastConfigIndexFromBlock(block)
	if err != nil {
		return errors.Wrap(err, "failed to fetch last config block index from block")
	}
	if s.configBlockNumber != lastConfigBlockIdx {
		// Fabric's config block points to itself. If it is not a config block that
		// points to itself, then return an error.
		// We have a nested verification to ensure we don't run the relatively heavy
		// check [protoutil.IsConfigBlock()] for most cases.
		if !protoutil.IsConfigBlock(block) {
			return errors.Newf("block's last config block [%d] != [%d] current config block",
				lastConfigBlockIdx, s.configBlockNumber)
		}
		if lastConfigBlockIdx != blockNumber {
			return errors.Newf("config block's last config block [%d] != [%d] config block number",
				lastConfigBlockIdx, blockNumber)
		}
	}

	// We have a unique handling for the genesis block as Fabric's orderer does not sign it.
	// We verify that it is identical to the genesis block we have.
	// If we got block #0, we must also have the config block #0, as we assert that the config
	// block is never ahead of the next expected block.
	if blockNumber == 0 {
		ok := proto.Equal(block, s.ConfigBlock)
		if !ok {
			return errors.New("delivered genesis block mismatch to known genesis block")
		}
		return nil
	}

	// Verify block according to the config block policy.
	err = s.verifierFunc(block.Header, block.Metadata)
	return errors.Wrapf(err, "block signature verification failed on block [%d]", blockNumber)
}

// updateIfConfigBlock sets the config by which blocks are verified.
// It is assumed that this config block had already been
// verified using the VerifyBlock method immediately prior to calling this method.
func (cs *configState) updateIfConfigBlock(block *common.Block) error {
	configMaterial, err := channelconfig.LoadConfigBlockMaterial(block)
	if configMaterial == nil || err != nil {
		return err
	}

	// This is a config block. Let's start validating.
	if cs.ConfigBlockMaterial != nil && configMaterial.ChannelID != cs.ChannelID {
		return errors.Newf("config block channel ID [%s] does not match expected [%s]",
			configMaterial.ChannelID, cs.ChannelID)
	}

	verifierFunc, err := fetchVerifier(configMaterial.Bundle)
	if err != nil {
		return err
	}

	cs.ConfigBlockMaterial = configMaterial
	cs.configBlockNumber = block.Header.Number
	cs.verifierFunc = verifierFunc
	return nil
}

func fetchVerifier(bundle *channelconfig.Bundle) (protoutil.BlockVerifierFunc, error) {
	policy, exists := bundle.PolicyManager().GetPolicy(policies.BlockValidation)
	if !exists {
		return nil, errors.Newf("no `%s` policy in config block", policies.BlockValidation)
	}

	oc, ok := bundle.OrdererConfig()
	if !ok {
		return nil, errors.New("no orderer section in config block")
	}

	var consenters []*common.Consenter
	bftEnabled := bundle.ChannelConfig().Capabilities().ConsensusTypeBFT()
	if bftEnabled {
		consenters = oc.Consenters()
	}

	return protoutil.BlockSignatureVerifier(bftEnabled, consenters, policy), nil
}
