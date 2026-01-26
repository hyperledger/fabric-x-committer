/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package workload

import (
	"context"
	"testing"

	"github.com/hyperledger/fabric-x-common/api/committerpb"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/utils/signature"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

// GenerateTransactions is used for benchmarking.
func GenerateTransactions(tb testing.TB, p *Profile, count int) []*servicepb.LoadGenTx {
	tb.Helper()
	s := NewTxStream(p, &StreamOptions{
		BuffersSize: 1024,
		GenBatch:    4096,
	})
	ctx, cancel := context.WithCancel(tb.Context())
	defer cancel()
	test.RunServiceForTest(ctx, tb, s.Run, nil)
	return s.MakeGenerator().Consume(ctx, ConsumeParameters{
		RequestedItems: uint64(count), //nolint:gosec // int -> uint64.
	})
}

// DefaultProfile is used for testing and benchmarking.
func DefaultProfile(workers uint32) *Profile {
	return &Profile{
		Key: KeyProfile{Size: 32},
		// We use a small block to reduce the CPU load during tests.
		Block: BlockProfile{MaxSize: 10},
		Transaction: TransactionProfile{
			ReadWriteValueSize: 32,
			ReadWriteCount:     NewConstantDistribution(2),
		},
		Query: QueryProfile{
			QuerySize:             NewConstantDistribution(100),
			MinInvalidKeysPortion: NewConstantDistribution(0),
			Shuffle:               false,
		},
		Policy: PolicyProfile{
			NamespacePolicies: map[string]*Policy{
				DefaultGeneratedNamespaceID: {Scheme: signature.NoScheme},
				committerpb.MetaNamespaceID: {Scheme: signature.Ecdsa},
			},
		},
		Conflicts: ConflictProfile{
			InvalidSignatures: Never,
		},
		Seed:    249822374033311501,
		Workers: workers,
	}
}
