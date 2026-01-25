/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package verifier

import (
	"context"
	"time"

	"github.com/hyperledger/fabric-x-common/api/committerpb"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/utils"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
)

type (
	parallelExecutor struct {
		inputCh        chan *servicepb.TxWithRef
		outputSingleCh chan *verificationOutput
		outputCh       chan []*committerpb.TxStatus
		verifier       *verifier
		config         *ExecutorConfig
	}

	verificationOutput struct {
		status   *committerpb.TxStatus
		isConfig bool
	}
)

func newParallelExecutor(config *ExecutorConfig) *parallelExecutor {
	channelCapacity := config.ChannelBufferSize * config.Parallelism
	return &parallelExecutor{
		config:         config,
		inputCh:        make(chan *servicepb.TxWithRef, channelCapacity),
		outputCh:       make(chan []*committerpb.TxStatus),
		outputSingleCh: make(chan *verificationOutput, channelCapacity),
		verifier:       newVerifier(),
	}
}

func (e *parallelExecutor) handleChannelInput(ctx context.Context) {
	chIn := channel.NewReader(ctx, e.inputCh)
	chOut := channel.NewWriter(ctx, e.outputSingleCh)
	for {
		input, ok := chIn.Read()
		if !ok {
			return
		}
		logger.Debugf("Received request '%v' with in worker", &utils.LazyJSON{O: input.Ref})
		output := e.verifier.verifyRequest(input)
		logger.Debugf("Received output from executor: %v", output)
		chOut.Write(&verificationOutput{status: output, isConfig: utils.IsConfigTx(input.Content.Namespaces)})
	}
}

func (e *parallelExecutor) handleCutoff(ctx context.Context) {
	var outputBuffer []*committerpb.TxStatus
	chOut := channel.NewWriter(ctx, e.outputCh)
	cutBatch := func(size int) {
		for len(outputBuffer) >= size {
			batchSize := min(e.config.BatchSizeCutoff, len(outputBuffer))
			logger.Debugf("Cuts batch with %d/%d of the outputs.", batchSize, len(outputBuffer))
			chOut.Write(outputBuffer[:batchSize])
			outputBuffer = outputBuffer[batchSize:]
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(e.config.BatchTimeCutoff):
			logger.Debugf("Attempts to cut a batch (timout). (buffer size: %d)", len(outputBuffer))
			cutBatch(1)
		case output := <-e.outputSingleCh:
			logger.Debugf("Attempts to emit a batch (response). (buffer size: %d)", len(outputBuffer)+1)
			outputBuffer = append(outputBuffer, output.status)
			if output.isConfig {
				cutBatch(1)
			} else {
				cutBatch(e.config.BatchSizeCutoff)
			}
		}
	}
}
