/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package workload

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/utils/signature"
)

// GenerateTransactions is used for benchmarking and tests.
func GenerateTransactions(tb testing.TB, p *Profile, count int) []*servicepb.LoadGenTx {
	tb.Helper()
	if p == nil {
		p = DefaultProfile(1)
	}
	p.Workers = 1
	g := newIndependentTxGenerators(p)
	require.Len(tb, g, 1)
	return GenerateArray(g[0], count)
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
			},
		},
		Conflicts: ConflictProfile{
			InvalidSignatures: Never,
		},
		Seed:    249822374033311501,
		Workers: workers,
	}
}
