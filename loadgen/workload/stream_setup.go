/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package workload

import (
	"context"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
)

type (
	// StreamWithSetup implements the TxStream interface.
	StreamWithSetup struct {
		WorkloadSetupTXs channel.Reader[*servicepb.LoadGenTx]
		TxStream         *TxStream
	}

	// TxGeneratorWithSetup is a TX generator that first submit TXs from the WorkloadSetupTXs,
	// and blocks until indicated that it was committed.
	// Then, it submits transactions from the tx stream.
	TxGeneratorWithSetup struct {
		WorkloadSetupTXs channel.Reader[*servicepb.LoadGenTx]
		TxGen            *ConsumerRateController[*servicepb.LoadGenTx]
	}
)

// MakeTxGenerator instantiate clientTxGenerator.
func (c *StreamWithSetup) MakeTxGenerator() *TxGeneratorWithSetup {
	cg := &TxGeneratorWithSetup{WorkloadSetupTXs: c.WorkloadSetupTXs}
	if c.TxStream != nil {
		cg.TxGen = c.TxStream.MakeGenerator()
	}
	return cg
}

// Next generates the next TX batch.
func (g *TxGeneratorWithSetup) Next(ctx context.Context, p BlockProfile) []*servicepb.LoadGenTx {
	if g.WorkloadSetupTXs != nil {
		if tx, ok := g.WorkloadSetupTXs.Read(); ok {
			return []*servicepb.LoadGenTx{tx}
		}
		g.WorkloadSetupTXs = nil
	}
	if g.TxGen != nil {
		return g.TxGen.Consume(ctx, ConsumeParameters{
			RequestedItems: p.MaxSize,
			MinItems:       p.MinSize,
			SoftTimeout:    p.PreferredRate,
		})
	}
	return nil
}
