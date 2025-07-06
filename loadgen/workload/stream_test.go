/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package workload

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
	"github.com/hyperledger/fabric-x-committer/api/protoqueryservice"
	"github.com/hyperledger/fabric-x-committer/utils/signature"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

// result is used to prevent compiler optimizations.
var result float64

// printResult is used to prevent compiler optimizations.
func printResult() {
	fmt.Printf("Result: %v\n", result)
}

func defaultStreamOptions() *StreamOptions {
	// We set low values for the buffer and batch to reduce the CPU load during tests.
	return &StreamOptions{
		BuffersSize: 1,
		GenBatch:    1,
	}
}

func defaultBenchProfile(workers uint32) *Profile {
	p := DefaultProfile(workers)
	p.Block.Size = 1024
	p.Query.QuerySize = NewConstantDistribution(1024)
	return p
}

func defaultBenchStreamOptions() *StreamOptions {
	o := defaultStreamOptions()
	o.BuffersSize = 1000
	o.GenBatch = 4096
	return o
}

func benchWorkersProfiles() (profiles []*Profile) {
	for _, workers := range []uint32{1, 2, 4, 8, 16, 32, 64} {
		profiles = append(profiles, defaultBenchProfile(workers))
	}
	return profiles
}

func benchTxProfiles() (profiles []*Profile) {
	for _, sign := range []bool{true, false} {
		for _, p := range benchWorkersProfiles() {
			if !sign {
				p.Transaction.Policy.NamespacePolicies[GeneratedNamespaceID].Scheme = signature.NoScheme
			} else {
				p.Transaction.Policy.NamespacePolicies[GeneratedNamespaceID].Scheme = signature.Ecdsa
			}
			profiles = append(profiles, p)
		}
	}
	return profiles
}

func genericBench(b *testing.B, benchFunc func(b *testing.B, p *Profile)) {
	b.Helper()
	for _, p := range benchTxProfiles() {
		name := fmt.Sprintf("workers-%d-sign-%s",
			p.Workers, p.Transaction.Policy.NamespacePolicies[GeneratedNamespaceID].Scheme)
		b.Run(name, func(b *testing.B) {
			benchFunc(b, p)
		})
	}
	printResult()
}

func BenchmarkGenTx(b *testing.B) {
	//nolint:thelper // false positive.
	genericBench(b, func(b *testing.B, p *Profile) {
		t := NewTxStream(p, defaultBenchStreamOptions())

		b.ResetTimer()
		test.RunServiceForTest(b.Context(), b, t.Run, nil)
		g := t.MakeGenerator()

		var sum float64
		n := max(1, b.N/int(p.Block.Size)) //nolint:gosec // uint64 -> int.
		for range n {
			txs := g.NextN(b.Context(), int(p.Block.Size)) //nolint:gosec // uint64 -> int.
			sum += float64(len(txs))
		}
		b.StopTimer()

		// Prevent compiler optimizations.
		result += sum
	})
}

func BenchmarkGenQuery(b *testing.B) {
	//nolint:thelper // false positive.
	genericBench(b, func(b *testing.B, p *Profile) {
		c := NewQueryGenerator(p, defaultBenchStreamOptions())

		b.ResetTimer()
		test.RunServiceForTest(b.Context(), b, c.Run, nil)
		g := c.MakeGenerator()

		var sum float64
		n := max(1, b.N/int(p.Block.Size)) //nolint:gosec // uint64 -> int.
		for range n {
			q := g.NextN(b.Context(), int(p.Block.Size)) //nolint:gosec // uint64 -> int.
			sum += float64(len(q))
		}
		b.StopTimer()

		// Prevent compiler optimizations.
		result += sum
	})
}

func requireValidKey(t *testing.T, key []byte, profile *Profile) {
	t.Helper()
	require.Len(t, key, int(profile.Key.Size))
	require.Positive(t, SumInt(key))
}

func requireValidTx(t *testing.T, tx *protoblocktx.Tx, profile *Profile, signer *TxSignerVerifier) {
	t.Helper()
	require.Len(t, tx.Namespaces, 1)

	if profile.Transaction.ReadOnlyCount != nil {
		require.Len(t, tx.Namespaces[0].ReadsOnly, 1)
	} else {
		require.Empty(t, tx.Namespaces[0].ReadsOnly)
	}

	if profile.Transaction.ReadWriteCount != nil {
		require.Len(t, tx.Namespaces[0].ReadWrites, 2)
	} else {
		require.Empty(t, tx.Namespaces[0].ReadWrites)
	}

	if profile.Transaction.BlindWriteCount != nil {
		require.Len(t, tx.Namespaces[0].BlindWrites, 3)
	} else {
		require.Empty(t, tx.Namespaces[0].BlindWrites)
	}

	for _, v := range tx.Namespaces[0].ReadsOnly {
		requireValidKey(t, v.Key, profile)
	}

	for _, v := range tx.Namespaces[0].ReadWrites {
		requireValidKey(t, v.Key, profile)
	}

	for _, v := range tx.Namespaces[0].BlindWrites {
		requireValidKey(t, v.Key, profile)
	}

	require.True(t, signer.Verify(tx))
}

func testWorkersProfiles() (profiles []*Profile) {
	for _, workers := range []uint32{1, 2, 4, 8} {
		profiles = append(profiles, DefaultProfile(workers))
	}
	return profiles
}

func testTxProfiles() (profiles []*Profile) {
	for _, onlyReadWrite := range []bool{true, false} {
		for _, p := range testWorkersProfiles() {
			if !onlyReadWrite {
				p.Transaction.ReadOnlyCount = NewConstantDistribution(1)
				p.Transaction.BlindWriteCount = NewConstantDistribution(3)
			} else {
				p.Transaction.ReadOnlyCount = nil
				p.Transaction.BlindWriteCount = nil
			}
			profiles = append(profiles, p)
		}
	}
	return profiles
}

func startTxGeneratorUnderTest(
	t *testing.T, profile *Profile, options *StreamOptions, modifierGenerators ...Generator[Modifier],
) *TxStream {
	t.Helper()
	g := NewTxStream(profile, options, modifierGenerators...)
	test.RunServiceForTest(t.Context(), t, g.Run, nil)
	return g
}

func startQueryGeneratorUnderTest(
	t *testing.T, profile *Profile, options *StreamOptions,
) *RateLimiterGenerator[*protoqueryservice.Query] {
	t.Helper()
	g := NewQueryGenerator(profile, options)
	test.RunServiceForTest(t.Context(), t, g.Run, nil)
	return g.MakeGenerator()
}

func TestGenValidTx(t *testing.T) {
	t.Parallel()
	for _, p := range testTxProfiles() {
		p := p
		onlyReadWrite := p.Transaction.ReadOnlyCount == nil
		t.Run(fmt.Sprintf(
			"workers:%d-onlyReadWrite:%v", p.Workers, onlyReadWrite,
		), func(t *testing.T) {
			t.Parallel()
			c := startTxGeneratorUnderTest(t, p, defaultStreamOptions())
			g := c.MakeGenerator()
			signer := NewTxSignerVerifier(p.Transaction.Policy)

			for range 100 {
				requireValidTx(t, g.Next(t.Context()), p, signer)
			}
		})
	}
}

func TestGenValidBlock(t *testing.T) {
	t.Parallel()
	for _, p := range testTxProfiles() {
		p := p
		onlyReadWrite := p.Transaction.ReadOnlyCount == nil
		t.Run(fmt.Sprintf(
			"workers:%d-onlyReadWrite:%v", p.Workers, onlyReadWrite,
		), func(t *testing.T) {
			t.Parallel()
			c := startTxGeneratorUnderTest(t, p, defaultStreamOptions())
			g := c.MakeGenerator()
			signer := NewTxSignerVerifier(p.Transaction.Policy)

			for range 5 {
				txs := g.NextN(t.Context(), int(p.Block.Size)) //nolint:gosec // uint64 -> int.
				for _, tx := range txs {
					requireValidTx(t, tx, p, signer)
				}
			}
		})
	}
}

func TestGenInvalidSigTx(t *testing.T) {
	t.Parallel()
	p := DefaultProfile(1)
	p.Transaction.Policy.NamespacePolicies[GeneratedNamespaceID].Scheme = signature.Ecdsa
	p.Conflicts.InvalidSignatures = 0.2

	c := startTxGeneratorUnderTest(t, p, defaultStreamOptions())
	g := c.MakeGenerator()
	txs := g.NextN(t.Context(), 1e4)
	signer := NewTxSignerVerifier(p.Transaction.Policy)
	valid := Map(txs, func(_ int, _ *protoblocktx.Tx) float64 {
		if !signer.Verify(g.Next(t.Context())) {
			return 1
		}
		return 0
	})
	requireBernoulliDist(t, valid, 0.2, 1e-2)
}

func TestGenDependentTx(t *testing.T) {
	t.Parallel()
	p := DefaultProfile(1)
	p.Transaction.Policy.NamespacePolicies[GeneratedNamespaceID].Scheme = signature.NoScheme
	p.Conflicts.Dependencies = []DependencyDescription{
		{
			Gap:         NewConstantDistribution(1),
			Src:         "write",
			Dst:         "write",
			Probability: 0.1,
		},
		{
			Gap:         NewConstantDistribution(1),
			Src:         "read",
			Dst:         "write",
			Probability: 0.1,
		},
		{
			Gap:         NewConstantDistribution(1),
			Src:         "write",
			Dst:         "read-write",
			Probability: 0.1,
		},
		{
			Gap:         NewConstantDistribution(1),
			Src:         "read-write",
			Dst:         "read-write",
			Probability: 0.1,
		},
	}

	c := startTxGeneratorUnderTest(t, p, defaultStreamOptions())
	g := c.MakeGenerator()

	txs := g.NextN(t.Context(), 1e6)
	m := make(map[string]uint64)
	for _, tx := range txs {
		for _, ns := range tx.Namespaces {
			for _, r := range ns.ReadsOnly {
				m[string(r.Key)]++
			}
			for _, rw := range ns.ReadWrites {
				m[string(rw.Key)]++
			}
			for _, w := range ns.BlindWrites {
				m[string(w.Key)]++
			}
		}
	}

	var sum uint64
	for _, v := range m {
		sum += v - 1
	}
	require.InDelta(t, 0.4, float64(sum)/float64(len(txs)), 1e-3)
}

func TestBlindWriteWithValue(t *testing.T) {
	t.Parallel()
	p := DefaultProfile(1)
	p.Transaction.BlindWriteValueSize = 32
	p.Transaction.BlindWriteCount = NewConstantDistribution(2)

	c := startTxGeneratorUnderTest(t, p, defaultStreamOptions())
	g := c.MakeGenerator()
	tx := g.Next(t.Context())
	require.Len(t, tx.Namespaces[0].BlindWrites, 2)
	for _, v := range tx.Namespaces[0].BlindWrites {
		require.Len(t, v.Value, 32)
	}
}

func TestReadWriteWithValue(t *testing.T) {
	t.Parallel()
	p := DefaultProfile(1)
	p.Transaction.ReadWriteValueSize = 32
	p.Transaction.ReadWriteCount = NewConstantDistribution(2)

	c := startTxGeneratorUnderTest(t, p, defaultStreamOptions())
	g := c.MakeGenerator()
	tx := g.Next(t.Context())
	require.Len(t, tx.Namespaces[0].ReadWrites, 2)
	for _, v := range tx.Namespaces[0].ReadWrites {
		require.Len(t, v.Value, 32)
	}
}

func TestGenTxWithRateLimit(t *testing.T) {
	t.Parallel()
	p := DefaultProfile(8)
	limit := 100
	expectedSeconds := 5
	producedTotal := expectedSeconds * limit

	options := defaultStreamOptions()
	options.RateLimit = &LimiterConfig{InitialLimit: rate.Limit(limit)}
	options.GenBatch = uint32(producedTotal) //nolint:gosec // int -> uint32.
	c := startTxGeneratorUnderTest(t, p, options)
	g := c.MakeGenerator()

	// First burst is unlimited.
	g.NextN(t.Context(), producedTotal)

	start := time.Now()
	g.NextN(t.Context(), producedTotal)
	duration := time.Since(start)
	require.InDelta(t, float64(expectedSeconds), duration.Seconds(), 0.1)
}

// modGenTester simulates querying the version from the query service.
type modGenTester struct {
	nsVersion uint64
}

func (m *modGenTester) Next() Modifier {
	return m
}

func (m *modGenTester) Modify(tx *protoblocktx.Tx) (*protoblocktx.Tx, error) {
	for _, ns := range tx.Namespaces {
		ns.NsVersion = m.nsVersion
	}
	return tx, nil
}

func TestGenTxWithModifier(t *testing.T) {
	t.Parallel()
	p := DefaultProfile(8)

	mod0 := &modGenTester{0}
	mod1 := &modGenTester{1}
	c := startTxGeneratorUnderTest(t, p, defaultStreamOptions(), mod0, mod1)
	g := c.MakeGenerator()
	tx := g.Next(t.Context())
	for _, ns := range tx.Namespaces {
		require.Equal(t, mod1.nsVersion, ns.NsVersion)
	}
}

type queryTestEnv struct {
	p        *Profile
	keys     map[string]*struct{}
	txGen    *RateLimiterGenerator[*protoblocktx.Tx]
	queryGen *RateLimiterGenerator[*protoqueryservice.Query]
}

func newQueryTestEnv(t *testing.T, p *Profile, o *StreamOptions) *queryTestEnv {
	t.Helper()
	q := &queryTestEnv{
		p:        p,
		keys:     make(map[string]*struct{}),
		txGen:    startTxGeneratorUnderTest(t, p, o).MakeGenerator(),
		queryGen: startQueryGeneratorUnderTest(t, p, o),
	}
	for range 100 {
		q.addBlock(t.Context(), p.Block.Size)
	}
	return q
}

func (q *queryTestEnv) addBlock(ctx context.Context, size uint64) {
	txs := q.txGen.NextN(ctx, int(size)) //nolint:gosec // uint64 -> int.
	for _, tx := range txs {
		for _, ns := range tx.Namespaces {
			for _, r := range ns.ReadsOnly {
				q.keys[string(r.Key)] = nil
			}
			for _, rw := range ns.ReadWrites {
				q.keys[string(rw.Key)] = nil
			}
			for _, w := range ns.BlindWrites {
				q.keys[string(w.Key)] = nil
			}
		}
	}
}

func (q *queryTestEnv) exists(key []byte) bool {
	_, ok := q.keys[string(key)]
	return ok
}

func (q *queryTestEnv) countExistingKeys(keys [][]byte) int {
	if q.keys == nil {
		return 0
	}
	c := 0
	for _, k := range keys {
		if q.exists(k) {
			c++
		}
	}
	return c
}

func TestQuery(t *testing.T) {
	t.Parallel()
	for _, p := range testWorkersProfiles() {
		p := p
		t.Run(fmt.Sprintf("workers:%d", p.Workers), func(t *testing.T) {
			t.Parallel()
			env := newQueryTestEnv(t, p, defaultStreamOptions())

			for i := range 5 {
				query := env.queryGen.Next(t.Context())
				// Since the blocks are generated in parallel, the order of the
				// keys in the block might not be the same as in the query.
				// So we need to consume blocks until we found all the keys.
				require.Eventuallyf(t, func() bool {
					env.addBlock(t.Context(), p.Block.Size)
					return len(query.Namespaces[0].Keys) == env.countExistingKeys(query.Namespaces[0].Keys)
				}, time.Second*5, 1, "iteration %d", i)
			}
		})
	}
}

func TestQueryWithInvalid(t *testing.T) {
	t.Parallel()
	for _, portion := range []float64{0.1, 0.5, 0.7} {
		portion := portion
		t.Run(fmt.Sprintf("invalid-portion:%.1f", portion), func(t *testing.T) {
			t.Parallel()
			p := DefaultProfile(1)
			p.Query.MinInvalidKeysPortion = NewConstantDistribution(portion)
			env := newQueryTestEnv(t, p, defaultStreamOptions())

			existing := 0
			total := 0
			for range 10 {
				query := env.queryGen.Next(t.Context())
				total += len(query.Namespaces[0].Keys)
				existing += env.countExistingKeys(query.Namespaces[0].Keys)
			}

			require.InDelta(t, portion, float64(total-existing)/float64(total), 1e-3)
		})
	}
}

func TestQueryShuffle(t *testing.T) {
	t.Parallel()
	portion := 0.5
	t.Run("no-shuffle", func(t *testing.T) {
		t.Parallel()
		p := DefaultProfile(1)
		p.Query.MinInvalidKeysPortion = NewConstantDistribution(portion)
		p.Query.Shuffle = false
		env := newQueryTestEnv(t, p, defaultStreamOptions())

		for range 5 {
			query := env.queryGen.Next(t.Context())
			validCount := int(math.Round(float64(len(query.Namespaces[0].Keys)) * portion))
			require.Equal(t, validCount, env.countExistingKeys(query.Namespaces[0].Keys[:validCount]))
			require.Equal(t, 0, env.countExistingKeys(query.Namespaces[0].Keys[validCount:]))
		}
	})

	t.Run("with-shuffle", func(t *testing.T) {
		t.Parallel()
		p := DefaultProfile(1)
		p.Query.MinInvalidKeysPortion = NewConstantDistribution(portion)
		p.Query.Shuffle = true
		env := newQueryTestEnv(t, p, defaultStreamOptions())

		for range 5 {
			query := env.queryGen.Next(t.Context())
			validCount := int(math.Round(float64(len(query.Namespaces[0].Keys)) * portion))
			require.Equal(t, validCount, env.countExistingKeys(query.Namespaces[0].Keys))
			require.NotEqual(t, validCount, env.countExistingKeys(query.Namespaces[0].Keys[:validCount]))
			require.NotEqual(t, 0, env.countExistingKeys(query.Namespaces[0].Keys[validCount:]))
		}
	})
}
