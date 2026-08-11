/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package workload

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hyperledger/fabric-lib-go/common/flogging"
	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/hyperledger/fabric-x-common/msp"
	"github.com/hyperledger/fabric-x-common/protoutil"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/utils/serialization"
	"github.com/hyperledger/fabric-x-committer/utils/signature"
	"github.com/hyperledger/fabric-x-committer/utils/test"
	"github.com/hyperledger/fabric-x-committer/utils/testsig"
)

func defaultStreamOptions() *StreamOptions {
	// We set low values for the buffer and batch to reduce the CPU load during tests.
	return &StreamOptions{
		BuffersSize: 1,
		GenBatch:    1,
	}
}

func defaultBenchProfile(workers uint32) *Profile {
	p := DefaultProfile(workers)
	p.Block.MaxSize = 1024
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
				p.Policy.NamespacePolicies[DefaultGeneratedNamespaceID].Scheme = signature.NoScheme
			} else {
				p.Policy.NamespacePolicies[DefaultGeneratedNamespaceID].Scheme = signature.Ecdsa
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
			p.Workers, p.Policy.NamespacePolicies[DefaultGeneratedNamespaceID].Scheme)
		b.Run(name, func(b *testing.B) {
			benchFunc(b, p)
		})
	}
}

func BenchmarkGenTx(b *testing.B) {
	flogging.ActivateSpec("fatal")
	//nolint:thelper // false positive.
	genericBench(b, func(b *testing.B, p *Profile) {
		t, err := NewTxStream(p, defaultBenchStreamOptions(), NewTxCounter(p.Transaction))
		require.NoError(b, err)

		ctx := b.Context()
		// Start the timer before creating the service: the stream generates
		// transactions in the background as soon as it starts, and that
		// generation is exactly the workload we want to measure. Resetting the
		// timer after startup would let the service pre-produce transactions
		// that the consume loop then reads "for free", inflating the reported
		// throughput.
		b.ResetTimer()
		test.RunServiceForTest(ctx, b, t.Run, nil)
		g := t.MakeGenerator()

		param := ConsumeParameters{MinItems: p.Block.MinSize}
		var sum int
		for sum < b.N {
			param.RequestedItems = min(p.Block.MaxSize, uint64(b.N-sum)) //nolint:gosec // uint64 -> int.
			txs := g.Consume(ctx, param)
			sum += len(txs)
		}
		b.StopTimer()
		test.ReportTxPerSecond(b)
	})
}

func requireValidKey(t *testing.T, key []byte, profile *Profile) {
	t.Helper()
	require.Len(t, key, int(profile.Transaction.KeySize))
	require.Positive(t, SumInt(key))
}

func requireValidTx(t *testing.T, tx *servicepb.LoadGenTx, profile *Profile, endorser *TxEndorser) {
	t.Helper()
	require.NotEmpty(t, tx.Id)
	require.NotNil(t, tx.Tx)
	require.NotEmpty(t, tx.EnvelopePayload)
	require.Len(t, tx.Tx.Namespaces, 1)

	require.Len(t, tx.Tx.Namespaces[0].ReadsOnly, int(profile.Transaction.ReadOnlyCount))
	require.Len(t, tx.Tx.Namespaces[0].ReadWrites, int(profile.Transaction.ReadWriteCount))
	require.Len(t, tx.Tx.Namespaces[0].BlindWrites, int(profile.Transaction.BlindWriteCount))

	for _, v := range tx.Tx.Namespaces[0].ReadsOnly {
		requireValidKey(t, v.Key, profile)
	}

	for _, v := range tx.Tx.Namespaces[0].ReadWrites {
		requireValidKey(t, v.Key, profile)
	}

	for _, v := range tx.Tx.Namespaces[0].BlindWrites {
		requireValidKey(t, v.Key, profile)
	}

	require.True(t, verify(t, endorser.VerificationPolicies(), tx.Id, tx.Tx, nil))
}

func testWorkersProfiles() (profiles []*Profile) {
	for _, workers := range []uint32{1, 2, 4, 8} {
		profiles = append(profiles, DefaultProfile(workers))
	}
	return profiles
}

func testTxProfiles(t *testing.T) (profiles []*Profile) {
	t.Helper()
	for _, onlyReadWrite := range []bool{true, false} {
		for _, p := range testWorkersProfiles() {
			if !onlyReadWrite {
				p.Transaction.ReadOnlyCount = 1
				p.Transaction.BlindWriteCount = 3
			} else {
				p.Transaction.ReadOnlyCount = 0
				p.Transaction.BlindWriteCount = 0
			}
			profiles = append(profiles, p)
		}
	}

	// Adding test cases with user keys.
	tmpDir := t.TempDir()

	sigKey, verKey := testsig.NewKeyPair(signature.Ecdsa)
	sigPath := KeyPath{
		SigningKey:      filepath.Join(tmpDir, "signing.key"),
		VerificationKey: filepath.Join(tmpDir, "verification.key"),
	}
	require.NoError(t, os.WriteFile(sigPath.SigningKey, sigKey, 0o600))
	require.NoError(t, os.WriteFile(sigPath.VerificationKey, verKey, 0o600))
	sigProfile := DefaultProfile(1)
	sigProfile.Policy.NamespacePolicies[DefaultGeneratedNamespaceID].KeyPath = &sigPath

	sigWithCertKey, cert := testsig.EcdsaNewKeyPairWithCert()
	sigWithCertPath := KeyPath{
		SigningKey:      filepath.Join(tmpDir, "signing-with-cert.key"),
		SignCertificate: filepath.Join(tmpDir, "cert.pem"),
	}
	require.NoError(t, os.WriteFile(sigWithCertPath.SigningKey, sigWithCertKey, 0o600))
	require.NoError(t, os.WriteFile(sigWithCertPath.SignCertificate, cert, 0o600))
	sigWithCertProfile := DefaultProfile(1)
	sigWithCertProfile.Policy.NamespacePolicies[DefaultGeneratedNamespaceID].KeyPath = &sigWithCertPath

	return append(profiles, sigProfile, sigWithCertProfile)
}

func startTxGeneratorUnderTest(t *testing.T, profile *Profile, options *StreamOptions) *TxStream {
	t.Helper()
	g, err := NewTxStream(profile, options, NewTxCounter(profile.Transaction))
	require.NoError(t, err)
	test.RunServiceForTest(t.Context(), t, g.Run, nil)
	return g
}

// firstGen builds a single generator with a fresh counter, failing the test on error — the common
// single-generator case for tests and benchmarks.
func firstGen(tb testing.TB, p *Profile) *IndependentTxGenerator {
	tb.Helper()
	gens, err := newIndependentTxGenerators(p, NewTxCounter(p.Transaction))
	require.NoError(tb, err)
	require.NotEmpty(tb, gens)
	return gens[0]
}

func TestGenValidTx(t *testing.T) {
	t.Parallel()
	for _, p := range testTxProfiles(t) {
		t.Run(profileTestName(p), func(t *testing.T) {
			t.Parallel()
			c := startTxGeneratorUnderTest(t, p, defaultStreamOptions())
			g := c.MakeGenerator()
			endorser := NewTxEndorser(&p.Policy)

			for range 100 {
				requireValidTx(t, g.Next(t.Context()), p, endorser)
			}
		})
	}
}

func TestTxStreamKeyStats(t *testing.T) {
	t.Parallel()
	p := DefaultProfile(1)
	p.Transaction.ReadOnlyCount = 1
	p.Transaction.ReadWriteCount = 2
	p.Transaction.BlindWriteCount = 1
	p.Transaction.KeyBackrefRate = 2.5 // 2.5 backrefs / tx of 4 slots => 1.5 fresh keys / tx
	p.Transaction.TxReferenceGap = 5
	p.Transaction.KeyLookbackWindow = 100

	counter := NewTxCounter(p.Transaction)
	s, err := NewTxStream(p, defaultStreamOptions(), counter)
	require.NoError(t, err)
	require.Equal(t, KeyStats{}, counter.KeyStats(), "nothing generated yet")

	const n = 100
	s.gens[0].buildBatch(n) // advances the shared counter by n

	w := uint64(p.Transaction.ReadWriteCount + p.Transaction.BlindWriteCount)
	ro := uint64(p.Transaction.ReadOnlyCount)
	require.Equal(t, KeyStats{
		KeyFrontier:         150,       // floor(100*1.5)
		ReferencedReadKeys:  n * ro,    // every read-only slot reuses a key
		ReferencedWriteKeys: n*w - 150, // the write slots that didn't create reuse a key
	}, counter.KeyStats())
}

// TestTxStreamKeyStatsAboveWriteCap covers a new-key rate above the write-slot count: the surplus new
// keys fall on read-only slots, and the reference counts must not underflow.
func TestTxStreamKeyStatsAboveWriteCap(t *testing.T) {
	t.Parallel()
	p := DefaultProfile(1)
	p.Transaction.ReadOnlyCount = 2
	p.Transaction.ReadWriteCount = 1
	p.Transaction.BlindWriteCount = 1
	p.Transaction.KeyBackrefRate = 1 // 1 backref / tx of 4 slots => 3 fresh keys / tx > 2 write slots
	p.Transaction.TxReferenceGap = 0
	p.Transaction.KeyLookbackWindow = 8
	require.NoError(t, p.Transaction.Validate())

	counter := NewTxCounter(p.Transaction)
	s, err := NewTxStream(p, defaultStreamOptions(), counter)
	require.NoError(t, err)
	const n = 100
	s.gens[0].buildBatch(n)
	// 300 new keys but only N*W=200 write slots to create them; the surplus 100 fall on read-only slots,
	// leaving 200-100=100 read-only slots reusing keys and none on the write slots.
	require.Equal(t, KeyStats{
		KeyFrontier:         300,
		ReferencedReadKeys:  100,
		ReferencedWriteKeys: 0,
	}, counter.KeyStats())
}

// TestTxStreamKeyStatsHistorical covers the default backref rate of zero: every slot introduces a fresh
// key, so there are no references and every slot counts as created (N*slotsPerTx).
func TestTxStreamKeyStatsHistorical(t *testing.T) {
	t.Parallel()
	p := DefaultProfile(1)
	p.Transaction.ReadOnlyCount = 1
	p.Transaction.ReadWriteCount = 2
	p.Transaction.BlindWriteCount = 1
	// KeyBackrefRate defaults to 0 => every slot is a fresh key, no references.

	counter := NewTxCounter(p.Transaction)
	s, err := NewTxStream(p, defaultStreamOptions(), counter)
	require.NoError(t, err)
	const n = 100
	s.gens[0].buildBatch(n)
	w := uint64(p.Transaction.ReadWriteCount + p.Transaction.BlindWriteCount)
	slotsPerTx := w + uint64(p.Transaction.ReadOnlyCount)
	require.Equal(t, KeyStats{KeyFrontier: n * slotsPerTx}, counter.KeyStats())
}

func TestGenValidBlock(t *testing.T) {
	t.Parallel()
	for _, p := range testTxProfiles(t) {
		t.Run(profileTestName(p), func(t *testing.T) {
			t.Parallel()
			c := startTxGeneratorUnderTest(t, p, defaultStreamOptions())
			g := c.MakeGenerator()
			endorser := NewTxEndorser(&p.Policy)

			for range 5 {
				txs := g.Consume(t.Context(), ConsumeParameters{RequestedItems: p.Block.MaxSize})
				for _, tx := range txs {
					requireValidTx(t, tx, p, endorser)
				}
			}
		})
	}
}

func profileTestName(p *Profile) string {
	onlyReadWrite := p.Transaction.ReadOnlyCount == 0
	key := "no-key"
	policy := p.Policy.NamespacePolicies[DefaultGeneratedNamespaceID]
	if policy.KeyPath != nil && policy.KeyPath.VerificationKey != "" {
		key = "verification-key"
	}
	if policy.KeyPath != nil && policy.KeyPath.SignCertificate != "" {
		key = "signing-certificate"
	}
	return fmt.Sprintf("workers:%d-onlyReadWrite:%v-%s", p.Workers, onlyReadWrite, key)
}

func TestGenInvalidSigTx(t *testing.T) {
	t.Parallel()
	p := DefaultProfile(1)
	p.Policy.NamespacePolicies[DefaultGeneratedNamespaceID].Scheme = signature.Ecdsa
	p.Transaction.InvalidSignatures = 0.2

	c := startTxGeneratorUnderTest(t, p, defaultStreamOptions())
	g := c.MakeGenerator()
	txs := g.Consume(t.Context(), ConsumeParameters{RequestedItems: 1e4})
	endorser := NewTxEndorser(&p.Policy)
	valid := Map(txs, func(_ int, tx *servicepb.LoadGenTx) float64 {
		if !verify(t, endorser.VerificationPolicies(), tx.Id, tx.Tx, nil) {
			return 1
		}
		return 0
	})
	requireBernoulliDist(t, valid, 0.2, 1e-2)
}

// TestGenerationContentIsBatchSizeInvariant proves generation content is a pure function of the global
// tx index, independent of batch granularity: for a fixed seed, n single-tx batches yield the same
// deterministic content, per index, as one batch of n — the TX ID and the inner Tx's Namespaces and
// Metadata. Endorsements and the envelope are excluded: they embed a non-deterministic ECDSA signature
// and envelope timestamp, so two independently-signed txs with identical content still differ there.
func TestGenerationContentIsBatchSizeInvariant(t *testing.T) {
	t.Parallel()
	p := DefaultProfile(1)
	p.Transaction.ReadOnlyCount = 1
	p.Transaction.ReadWriteCount = 2
	p.Transaction.BlindWriteCount = 1
	p.Transaction.MetadataSize = 16

	const n = 25
	singleGen := firstGen(t, p)
	wantTxs := make([]*servicepb.LoadGenTx, n)
	for i := range wantTxs {
		wantTxs[i] = singleGen.buildAndSignBatch(1)[0]
	}

	batchGen := firstGen(t, p)
	gotTxs := batchGen.buildAndSignBatch(n)
	require.Len(t, gotTxs, n)

	for i := range n {
		require.Equal(t, wantTxs[i].Id, gotTxs[i].Id, "tx %d: ID mismatch", i)
		test.RequireProtoEqual(t, deterministicTx(wantTxs[i].Tx), deterministicTx(gotTxs[i].Tx))
	}
}

// TestQueryStageRunsBeforeSign proves the query stage runs BEFORE signing: a fake querier stamps a
// fixed version on every read of a built (unsigned) batch, and that version must then appear in the
// SIGNED payload — decoded from the envelope, not read off the in-memory Tx pointer, which would show
// the mutation even if it had (incorrectly) happened after signing marshaled the payload.
func TestQueryStageRunsBeforeSign(t *testing.T) {
	t.Parallel()
	const fixedVersion = uint64(42)

	p := DefaultProfile(1)
	p.Transaction.ReadOnlyCount = 1
	p.Transaction.ReadWriteCount = 1

	g := firstGen(t, p)
	const n = 5
	built, base := g.buildBatch(n)
	require.NoError(t, (&fakeVersionQuerier{version: fixedVersion}).FillVersions(t.Context(), built))
	txs := g.signBatch(built, base)
	require.Len(t, txs, n)

	for _, tx := range txs {
		signedTx := decodeSignedTx(t, tx)
		for _, ns := range signedTx.Namespaces {
			for _, r := range ns.ReadsOnly {
				require.Equal(t, fixedVersion, r.GetVersion())
			}
			for _, rw := range ns.ReadWrites {
				require.Equal(t, fixedVersion, rw.GetVersion())
			}
		}
	}
}

// TestGenerationProducesValidSignedTxs covers build + sign with no query stage (the path Run's worker
// loop takes when no querier is wired): the batch is well-formed and validly signed.
func TestGenerationProducesValidSignedTxs(t *testing.T) {
	t.Parallel()
	p := DefaultProfile(1)
	p.Transaction.ReadOnlyCount = 1
	p.Transaction.ReadWriteCount = 2
	p.Transaction.BlindWriteCount = 1
	p.Policy.NamespacePolicies[DefaultGeneratedNamespaceID].Scheme = signature.Ecdsa
	endorser := NewTxEndorser(&p.Policy)

	g := firstGen(t, p)
	const n = 10
	txs := g.buildAndSignBatch(n)
	require.Len(t, txs, n)
	for _, tx := range txs {
		requireValidTx(t, tx, p, endorser)
	}
}

// txKeysByRole extracts the keys of a generated TX's single namespace, split by slot role, for tests
// that assert on key reuse across a stream.
func txKeysByRole(tx *servicepb.LoadGenTx) (readOnly, readWrite, blindWrite [][]byte) {
	ns := tx.Tx.Namespaces[0]
	for _, r := range ns.ReadsOnly {
		readOnly = append(readOnly, r.Key)
	}
	for _, rw := range ns.ReadWrites {
		readWrite = append(readWrite, rw.Key)
	}
	for _, w := range ns.BlindWrites {
		blindWrite = append(blindWrite, w.Key)
	}
	return readOnly, readWrite, blindWrite
}

func TestGenSplitContention(t *testing.T) {
	t.Parallel()
	p := DefaultProfile(1)
	p.Policy.NamespacePolicies[DefaultGeneratedNamespaceID].Scheme = signature.NoScheme
	// One backward reference per transaction: with 2 slots, key-backref-rate 1 leaves one create and one
	// reference one tx behind, drawn from a 2-key window so warmup adds at most a couple of extra
	// (negative) keys.
	p.Transaction.ReadWriteCount = 2
	p.Transaction.KeyBackrefRate = 1
	p.Transaction.TxReferenceGap = 1
	p.Transaction.KeyLookbackWindow = 2

	c := startTxGeneratorUnderTest(t, p, defaultStreamOptions())
	g := c.MakeGenerator()

	const n = 1000
	txs := g.Consume(t.Context(), ConsumeParameters{RequestedItems: n})
	require.Len(t, txs, n)

	distinct := make(map[string]struct{})
	for _, tx := range txs {
		_, rw, _ := txKeysByRole(tx)
		require.Len(t, rw, 2)
		for _, k := range rw {
			distinct[string(k)] = struct{}{}
		}
	}
	// n transactions create ~n keys total (one new per tx), not 2n: the second slot reuses an
	// existing key. Allow a small warmup slack.
	require.Less(t, len(distinct), n+10)
	require.Greater(t, len(distinct), n-10)
}

func TestBlindWriteWithValue(t *testing.T) {
	t.Parallel()
	p := DefaultProfile(1)
	p.Transaction.BlindWriteValueSize = 32
	p.Transaction.BlindWriteCount = 2

	c := startTxGeneratorUnderTest(t, p, defaultStreamOptions())
	g := c.MakeGenerator()
	tx := g.Next(t.Context())
	require.Len(t, tx.Tx.Namespaces[0].BlindWrites, 2)
	for _, v := range tx.Tx.Namespaces[0].BlindWrites {
		require.Len(t, v.Value, 32)
	}
}

func TestReadWriteWithValue(t *testing.T) {
	t.Parallel()
	p := DefaultProfile(1)
	p.Transaction.ReadWriteValueSize = 32
	p.Transaction.ReadWriteCount = 3

	c := startTxGeneratorUnderTest(t, p, defaultStreamOptions())
	g := c.MakeGenerator()
	tx := g.Next(t.Context())
	require.Len(t, tx.Tx.Namespaces[0].ReadWrites, 3)
	for _, v := range tx.Tx.Namespaces[0].ReadWrites {
		require.Len(t, v.Value, 32)
	}
}

func TestWithMetadata(t *testing.T) {
	t.Parallel()
	p := DefaultProfile(1)
	p.Transaction.MetadataSize = 128

	c := startTxGeneratorUnderTest(t, p, defaultStreamOptions())
	g := c.MakeGenerator()
	tx := g.Next(t.Context())
	require.Len(t, tx.Tx.Metadata, 1)
	require.Len(t, tx.Tx.Metadata[0], 128)
}

func TestGenTxWithRateLimit(t *testing.T) {
	t.Parallel()
	rate := uint64(1_000)
	expectedSeconds := 5
	producedTotal := expectedSeconds * int(rate)

	options := defaultStreamOptions()
	p := DefaultProfile(1)
	options.RateLimit = rate
	options.GenBatch = uint32(producedTotal)
	c := startTxGeneratorUnderTest(t, p, options)
	g := c.MakeGenerator()

	ctx, cancel := context.WithTimeout(t.Context(), time.Second*time.Duration(expectedSeconds*2))
	t.Cleanup(cancel)
	start := time.Now()
	txs := make([]*servicepb.LoadGenTx, 0, producedTotal)
	for len(txs) < producedTotal && ctx.Err() == nil {
		//nolint:gosec // uint64 -> int.
		request := min(p.Block.MaxSize, uint64(producedTotal-len(txs)))
		res := g.Consume(ctx, ConsumeParameters{RequestedItems: request})
		require.NotEmpty(t, res)
		txs = append(txs, res...)
	}
	duration := time.Since(start)
	require.InDelta(t, float64(expectedSeconds), duration.Seconds(), 0.2*float64(expectedSeconds))
}

func TestAsnMarshal(t *testing.T) {
	t.Parallel()
	loadGenTxs := GenerateTransactions(t, nil, 128)
	txs := make([]*applicationpb.TestTx, len(loadGenTxs))
	for i, tx := range loadGenTxs {
		txs[i] = &applicationpb.TestTx{
			ID:         tx.Id,
			Namespaces: tx.Tx.Namespaces,
		}
	}
	// We test against the generated load to enforce a coupling between different parts of the system.
	applicationpb.CommonTestAsnMarshal(t, txs)
}

//nolint:revive // 5 arguments.
func verify(
	t *testing.T,
	policies map[string]*applicationpb.NamespacePolicy,
	txID string, tx *applicationpb.Tx,
	idDeserializer msp.IdentityDeserializer,
) bool {
	t.Helper()
	if len(tx.Endorsements) < len(tx.Namespaces) {
		return false
	}
	for nsIndex, ns := range tx.Namespaces {
		policy, ok := policies[ns.NsId]
		require.Truef(t, ok, "No policy nsID=%s", ns.NsId)
		verifier, err := signature.NewNsVerifier(policy, idDeserializer)
		require.NoError(t, err, "Failed to create verifier for nsID=%s", ns.NsId)
		if verErr := verifier.VerifyNs(txID, tx, nsIndex); verErr != nil {
			return false
		}
	}
	return true
}

// requireBernoulliDist asserts that the given sample of 0/1 values has the
// expected proportion of 1s within delta.
func requireBernoulliDist(t *testing.T, sample []float64, probability Probability, delta float64) {
	t.Helper()
	var ones float64
	for _, v := range sample {
		ones += v
	}
	require.InDelta(t, probability, ones/float64(len(sample)), delta)
}

// deterministicTx strips a generated Tx down to the parts that are a pure function of the
// transaction index — Namespaces and Metadata — dropping Endorsements, which carries a
// non-deterministic ECDSA signature (or an envelope timestamp, for the envelope as a whole).
func deterministicTx(tx *applicationpb.Tx) *applicationpb.Tx {
	return &applicationpb.Tx{
		Namespaces: tx.Namespaces,
		Metadata:   tx.Metadata,
	}
}

// decodeSignedTx decodes the applicationpb.Tx actually embedded in the TX's signed envelope
// payload, as opposed to reading the in-memory tx.Tx pointer, which would reflect a mutation
// applied at any point, even after signing.
func decodeSignedTx(t *testing.T, tx *servicepb.LoadGenTx) *applicationpb.Tx {
	t.Helper()
	payload, err := protoutil.UnmarshalPayload(tx.EnvelopePayload)
	require.NoError(t, err)
	inner, err := serialization.UnmarshalTx(payload.Data)
	require.NoError(t, err)
	return inner
}

// fakeVersionQuerier is a batchQuerier test double that stamps a fixed version on every read
// (ReadsOnly and ReadWrites) across the batch, letting tests prove the query stage runs before
// signing.
type fakeVersionQuerier struct {
	version uint64
}

// FillVersions implements batchQuerier.
func (q *fakeVersionQuerier) FillVersions(_ context.Context, batch []*applicationpb.Tx) error {
	for _, tx := range batch {
		for _, ns := range tx.Namespaces {
			for _, r := range ns.ReadsOnly {
				version := q.version
				r.Version = &version
			}
			for _, rw := range ns.ReadWrites {
				version := q.version
				rw.Version = &version
			}
		}
	}
	return nil
}
