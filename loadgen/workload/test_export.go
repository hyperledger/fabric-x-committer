/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package workload

import (
	"testing"

	"github.com/hyperledger/fabric-x-common/utils/testcrypto"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/utils/signature"
	"github.com/hyperledger/fabric-x-committer/utils/testsig"
)

// GenerateTransactions is used for benchmarking and tests.
func GenerateTransactions(tb testing.TB, p *Profile, count int) []*servicepb.LoadGenTx {
	tb.Helper()
	if p == nil {
		p = DefaultProfile(1)
	}
	p.Workers = 1
	g, err := newIndependentTxGenerators(p, NewTxCounter(p.Transaction))
	require.NoError(tb, err)
	require.Len(tb, g, 1)
	require.Positive(tb, count)
	return g[0].buildAndSignBatch(uint64(count)) //nolint:gosec // count is a non-negative test batch size.
}

// GenerateTransactionsConcurrently builds count transactions using workers generators at once. It
// is equivalent to GenerateTransactions apart from the order: every transaction is a pure function
// of a global index, and the generators hand those out from one shared TxCounter, so the same
// transactions are produced whichever way they are split.
//
// Use it wherever the transactions are setup rather than the thing being measured. Signing is the
// bulk of the cost, so a benchmark that needs millions of transactions before it can start spends
// most of its wall time — and most of any CPU profile taken over the run — in this helper unless it
// is spread across cores.
func GenerateTransactionsConcurrently(tb testing.TB, p *Profile, count, workers int) []*servicepb.LoadGenTx {
	tb.Helper()
	require.Positive(tb, count)
	require.Positive(tb, workers)
	if p == nil {
		p = DefaultProfile(1)
	}
	workers = min(workers, count)
	p.Workers = uint32(workers) //nolint:gosec // workers is a positive test worker count.
	gens, err := newIndependentTxGenerators(p, NewTxCounter(p.Transaction))
	require.NoError(tb, err)
	require.Len(tb, gens, workers)

	txs := make([]*servicepb.LoadGenTx, count)
	var g errgroup.Group
	for i, gen := range gens {
		start := count * i / workers
		end := count * (i + 1) / workers
		g.Go(func() error {
			copy(txs[start:end], gen.buildAndSignBatch(uint64(end-start))) //nolint:gosec // end > start.
			return nil
		})
	}
	require.NoError(tb, g.Wait())
	return txs
}

// DefaultProfile is used for testing and benchmarking.
func DefaultProfile(workers uint32) *Profile {
	return &Profile{
		// We use a small block to reduce the CPU load during tests.
		Block: BlockProfile{MaxSize: 10},
		Transaction: TransactionProfile{
			KeySize:            32,
			ReadWriteValueSize: 32,
			ReadWriteCount:     2,
		},
		Policy: PolicyProfile{
			NamespacePolicies: map[string]*Policy{
				DefaultGeneratedNamespaceID: {Scheme: signature.NoScheme},
			},
		},
		Seed:    249822374033311501,
		Workers: workers,
	}
}

// NewPolicyEndorserFromMsp creates an MSP-based endorser and namespace policy from the
// peer organization crypto artifacts under artifactsPath.
func NewPolicyEndorserFromMsp(tb testing.TB, artifactsPath string) *testsig.NsEndorser {
	tb.Helper()
	signingIdentities, err := testcrypto.GetPeersIdentities(artifactsPath)
	require.NoError(tb, err)
	endorser, _ := newPolicyEndorserFromMSP(signingIdentities)
	return endorser
}
