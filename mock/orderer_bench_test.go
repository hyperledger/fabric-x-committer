/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package mock

import (
	"context"
	"fmt"
	"testing"

	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-x-common/protoutil"
	"github.com/hyperledger/fabric-x-common/utils/testcrypto"
	"github.com/stretchr/testify/require"
)

// BenchmarkOrdererBlockRate measures how fast the mock orderer can serve blocks that were cut for it,
// which is the rate the load generator's sidecar adapter is bounded by.
//
// It is the generator, not the committer, that this bounds: at 500 transactions a block the adapter
// stalled near 850 blocks a second, so a committer that could take more was never asked to. The cost is
// all in preparing the block on one goroutine -- deep-cloning it and hashing its data -- and
// PrepareInPlace removes the clone and takes the hash from the header the submitter already filled in.
//
// Both block sizes are measured because the trade is between them: the same per-block cost is spread
// over twenty times as many transactions in a 10,000-transaction block.
func BenchmarkOrdererBlockRate(b *testing.B) {
	// The crypto material is generated once and shared: generating it per case costs far more than the
	// measurement, and it is identical for all of them. The orderer needs only the consenter signing
	// identities from it, since nothing here serves gRPC.
	artifactsPath := b.TempDir()
	_, err := testcrypto.CreateOrExtendConfigBlockWithCrypto(artifactsPath, &testcrypto.ConfigBlock{
		ChannelID:             "channel",
		PeerOrganizationCount: 1,
	})
	require.NoError(b, err)

	for _, txCount := range []int{500, 10_000} {
		for _, inPlace := range []bool{false, true} {
			b.Run(fmt.Sprintf("prepare-in-place=%v/tx=%d", inPlace, txCount), func(b *testing.B) {
				orderer, err := NewMockOrderer(&OrdererConfig{
					ArtifactsPath:    artifactsPath,
					SendGenesisBlock: false,
					BlockSize:        1,
					// Two blocks, deliberately: SubmitBlock only hands the block to the preparing
					// goroutine, so a deep buffer would measure the channel write and never the
					// preparation behind it. At a depth of two the submitter can run at most one block
					// ahead and is paced by the stage under test.
					OutBlockCapacity: 2,
					PrepareInPlace:   inPlace,
				})
				require.NoError(b, err)

				// A ring of distinct blocks, not one block submitted repeatedly. In-place preparation
				// writes the header into the block it was given, so a submitter that reuses one block
				// hands the orderer's cache several entries that are all the same object, and a consumer
				// asking for block n can wait for a number that has already been overwritten. That is
				// the contract PrepareInPlace documents.
				blocks := make([]*common.Block, 8)
				for i := range blocks {
					blocks[i] = benchBlock(txCount)
					if inPlace {
						// The work the adapter's mapper stage does one stage ahead of the orderer.
						blocks[i].Header.DataHash = protoutil.ComputeBlockDataHash(blocks[i].Data)
					}
				}
				benchmarkBlockRate(b, orderer, blocks)
			})
		}
	}
}

// benchmarkBlockRate submits blocks in a ring as fast as the orderer accepts them, reporting the rate
// it serves them at. txCount is taken from the blocks, which the caller built.
func benchmarkBlockRate(b *testing.B, orderer *Orderer, blocks []*common.Block) {
	b.Helper()
	txCount := len(blocks[0].Data.Data)

	ctx, cancel := context.WithCancel(b.Context())
	defer cancel()
	go func() { _ = orderer.Run(ctx) }()
	// The block cache holds OutBlockCapacity blocks, so without a consumer the submitter would measure
	// the cache filling once and then block forever.
	go func() {
		for n := uint64(0); ctx.Err() == nil; n++ {
			if _, err := orderer.GetBlock(ctx, n); err != nil {
				return
			}
		}
	}()

	i := 0
	b.ResetTimer()
	for b.Loop() {
		require.NoError(b, orderer.SubmitBlock(ctx, blocks[i%len(blocks)]))
		i++
	}
	b.StopTimer()
	perBlock := b.Elapsed().Seconds() / float64(b.N)
	b.ReportMetric(1/perBlock, "blocks/s")
	b.ReportMetric(float64(txCount)/perBlock/1000, "Ktx/s")
}

// benchBlock builds one block of txCount serialized envelopes of 300 bytes each, the transaction size
// the evaluation generates.
func benchBlock(txCount int) *common.Block {
	data := make([][]byte, txCount)
	for i := range data {
		payload := make([]byte, 300)
		for j := range payload {
			payload[j] = byte(i + j)
		}
		data[i] = protoutil.MarshalOrPanic(&common.Envelope{Payload: payload})
	}
	block := &common.Block{
		Header: &common.BlockHeader{},
		Data:   &common.BlockData{Data: data},
	}
	return block
}
