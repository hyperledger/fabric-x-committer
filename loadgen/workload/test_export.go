/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package workload

import (
	"context"
	"testing"
	"time"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
	"github.com/hyperledger/fabric-x-committer/api/types"
	"github.com/hyperledger/fabric-x-committer/utils/signature"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

// StartGenerator is used for benchmarking.
func StartGenerator(tb testing.TB, p *Profile) *RateLimiterGenerator[*protoblocktx.Tx] {
	tb.Helper()
	s := NewTxStream(p, &StreamOptions{
		BuffersSize: 1024,
		GenBatch:    4096,
	})
	test.RunServiceForTest(tb.Context(), tb, s.Run, nil)

	// Give the generator a head start.
	time.Sleep(3 * time.Second)

	return s.MakeGenerator()
}

// GenerateTransactions is used for benchmarking.
func GenerateTransactions(tb testing.TB, p *Profile, count int) []*protoblocktx.Tx {
	tb.Helper()
	s := NewTxStream(p, &StreamOptions{
		BuffersSize: 1024,
		GenBatch:    4096,
	})
	ctx, cancel := context.WithCancel(tb.Context())
	defer cancel()
	test.RunServiceForTest(ctx, tb, s.Run, nil)
	return s.MakeGenerator().NextN(ctx, count)
}

// DefaultProfile is used for testing and benchmarking.
func DefaultProfile(workers uint32) *Profile {
	return &Profile{
		Key: KeyProfile{Size: 32},
		// We use a small block to reduce the CPU load during tests.
		Block: BlockProfile{Size: 10},
		Transaction: TransactionProfile{
			ReadWriteValueSize: 32,
			ReadWriteCount:     NewConstantDistribution(2),
			Policy: &PolicyProfile{
				NamespacePolicies: map[string]*Policy{
					GeneratedNamespaceID:  {Scheme: signature.NoScheme},
					types.MetaNamespaceID: {Scheme: signature.Ecdsa},
				},
			},
		},
		Query: QueryProfile{
			QuerySize:             NewConstantDistribution(100),
			MinInvalidKeysPortion: NewConstantDistribution(0),
			Shuffle:               false,
		},
		Conflicts: ConflictProfile{
			InvalidSignatures: Never,
		},
		Seed:    249822374033311501,
		Workers: workers,
	}
}
