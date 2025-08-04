/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package workload

import (
	"context"

	"github.com/hyperledger/fabric-x-committer/api/protoloadgen"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
)

type (
	// StreamWithSetup implements the TxStream interface.
	StreamWithSetup struct {
		WorkloadSetupTXs channel.Reader[*protoloadgen.TX]
		TxStream         *TxStream
		BlockSize        uint64
	}

	// TxGeneratorWithSetup is a TX generator that first submit TXs from the WorkloadSetupTXs,
	// and blocks until indicated that it was committed.
	// Then, it submits transactions from the tx stream.
	TxGeneratorWithSetup struct {
		WorkloadSetupTXs channel.Reader[*protoloadgen.TX]
		TxGen            *RateLimiterGenerator[*protoloadgen.TX]
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

// Next generate the next TX.
func (g *TxGeneratorWithSetup) Next(ctx context.Context, size int) []*protoloadgen.TX {
	if g.WorkloadSetupTXs != nil {
		if tx, ok := g.WorkloadSetupTXs.Read(); ok {
			return []*protoloadgen.TX{tx}
		}
		g.WorkloadSetupTXs = nil
	}
	if g.TxGen != nil {
		return g.TxGen.NextN(ctx, size)
	}
	return nil
}
