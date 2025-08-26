/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package verifier

import (
	"context"
	"time"

	"github.com/hyperledger/fabric-x-committer/api/protosigverifierservice"
	"github.com/hyperledger/fabric-x-committer/utils"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
)

type (
	parallelExecutor struct {
		inputCh        chan *protosigverifierservice.Tx
		outputSingleCh chan *protosigverifierservice.Response
		outputCh       chan []*protosigverifierservice.Response
		verifier       *verifier
		config         *ExecutorConfig
	}
)

func newParallelExecutor(config *ExecutorConfig) *parallelExecutor {
	channelCapacity := config.ChannelBufferSize * config.Parallelism
	return &parallelExecutor{
		config:         config,
		inputCh:        make(chan *protosigverifierservice.Tx, channelCapacity),
		outputCh:       make(chan []*protosigverifierservice.Response),
		outputSingleCh: make(chan *protosigverifierservice.Response, channelCapacity),
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
		chOut.Write(output)
	}
}

func (e *parallelExecutor) handleCutoff(ctx context.Context) {
	var outputBuffer []*protosigverifierservice.Response
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
			outputBuffer = append(outputBuffer, output)
			cutBatch(e.config.BatchSizeCutoff)
		}
	}
}
