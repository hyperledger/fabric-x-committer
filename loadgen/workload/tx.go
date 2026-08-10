/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package workload

import (
	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-x-common/api/applicationpb"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/utils/testsig"
)

// invalidSignatureBytes is the dummy signature stamped on transactions selected to be invalid.
var invalidSignatureBytes = []byte("dummy")

// IndependentTxGenerator builds transactions from a txRandomProcess. The generator only assembles:
// it embeds the TransactionProfile (the fixed layout set sizes and value sizes), lays out the
// namespace, optionally stamps a dummy endorsement in place of a real signature, and hands off to
// the TxBuilder. The random content — keys, values, nonce/TX ID, and metadata — comes from the
// process and is a pure function of the transaction index, so a transaction is reproducible by
// index. Generation is deterministic and side-effect free: fetching reads' committed versions is a
// separate, context-bound stage the stream runs between buildBatch and signBatch.
type IndependentTxGenerator struct {
	TransactionProfile
	process   *txRandomProcess
	txCounter *TxCounter
	TxBuilder *TxBuilder
}

// DefaultGeneratedNamespaceID for now we're only generating transactions for a single namespace.
const DefaultGeneratedNamespaceID = "0"

// newIndependentTxGenerators creates one generator per worker, each with its own txRandomProcess (each
// process is driven from a single goroutine and reuses a seed buffer), all sharing one global tx-id
// counter. Worker-count invariance comes from the shared counter, not from sharing the process.
func newIndependentTxGenerators(profile *Profile, counter *TxCounter) ([]*IndependentTxGenerator, error) {
	gens := make([]*IndependentTxGenerator, profile.Workers)
	for i := range gens {
		txb, err := NewTxBuilderFromPolicy(&profile.Policy, nil)
		if err != nil {
			return nil, errors.Wrap(err, "failed to create tx builder")
		}
		gens[i] = &IndependentTxGenerator{
			process:            newTxRandomProcess(profile),
			txCounter:          counter,
			TxBuilder:          txb,
			TransactionProfile: profile.Transaction,
		}
	}
	return gens, nil
}

// buildAndSignBatch builds a batch of n transactions and signs them with no query stage — the direct
// path for callers that do not version reads (tests and benchmarks). The stream's worker instead runs
// buildBatch and signBatch separately so it can slot the optional query stage between them.
func (g *IndependentTxGenerator) buildAndSignBatch(n uint64) []*servicepb.LoadGenTx {
	txs, base := g.buildBatch(n)
	return g.signBatch(txs, base)
}

// buildBatch reserves n consecutive global tx indices with a SINGLE atomic increment — the shared
// counter is the only cross-worker contention point, so a batch touches it once instead of n times —
// and builds the n unsigned transactions. It returns the batch and its base index; signBatch derives
// each nonce (hence TX ID) from base+i. buildBatch does no I/O and takes no context: fetching reads'
// versions (the only context-bound step) is a separate stage the stream runs before signBatch.
func (g *IndependentTxGenerator) buildBatch(n uint64) (txs []*applicationpb.Tx, base uint64) {
	base = g.txCounter.reserve(n)
	txs = make([]*applicationpb.Tx, n)
	for i := range n {
		txs[i] = g.buildTx(base + i)
	}
	return txs, base
}

// signBatch signs each transaction of a batch built at the given base index, turning the unsigned txs
// into enveloped LoadGenTxs. The nonce (hence the TX ID) is derived from base+i, so it need not be
// carried per transaction alongside the tx.
func (g *IndependentTxGenerator) signBatch(txs []*applicationpb.Tx, base uint64) []*servicepb.LoadGenTx {
	signed := make([]*servicepb.LoadGenTx, len(txs))
	for i, tx := range txs {
		txIdx := base + uint64(i)
		if g.process.invalidSignature(txIdx) {
			// Pre-assigning prevents TxBuilder from re-signing the TX.
			tx.Endorsements = make([]*applicationpb.Endorsements, len(tx.Namespaces))
			for i := range tx.Namespaces {
				tx.Endorsements[i] = testsig.CreateEndorsementsForThresholdRule(invalidSignatureBytes)[0]
			}
		}
		signed[i] = g.TxBuilder.MakeTxWithNonce(g.process.nonce(txIdx), tx)
	}
	return signed
}

// buildTx assembles the unsigned transaction at the given global index: keys and their values come from
// the per-transaction slot layout (new creates + existing references, keyed by flat key index), and the
// metadata is addressed by the index — so the whole TX is a pure function of that index. The nonce
// (hence the TX ID) is derived from the same index by signBatch.
func (g *IndependentTxGenerator) buildTx(txIdx uint64) *applicationpb.Tx {
	keys := g.process.slotKeys(txIdx)

	ns := &applicationpb.TxNamespace{
		NsId:        DefaultGeneratedNamespaceID,
		NsVersion:   0,
		ReadsOnly:   make([]*applicationpb.Read, g.ReadOnlyCount),
		ReadWrites:  make([]*applicationpb.ReadWrite, g.ReadWriteCount),
		BlindWrites: make([]*applicationpb.Write, g.BlindWriteCount),
	}

	// Keys come from the flat key indices in `keys`; each write-site's value is keyed by that key index
	// AND this transaction's index, so an update to a key writes a value distinct from the create's.
	for i := range ns.ReadsOnly {
		ns.ReadsOnly[i] = &applicationpb.Read{Key: g.process.key(keys.readOnly[i])}
	}
	for i := range ns.ReadWrites {
		ns.ReadWrites[i] = &applicationpb.ReadWrite{
			Key:   g.process.key(keys.readWrite[i]),
			Value: g.process.value(txIdx, keys.readWrite[i], g.ReadWriteValueSize),
		}
	}
	for i := range ns.BlindWrites {
		ns.BlindWrites[i] = &applicationpb.Write{
			Key:   g.process.key(keys.blindWrite[i]),
			Value: g.process.value(txIdx, keys.blindWrite[i], g.BlindWriteValueSize),
		}
	}

	var metadata [][]byte
	if item := g.process.metadata(txIdx, g.MetadataSize); len(item) > 0 {
		metadata = [][]byte{item}
	}

	return &applicationpb.Tx{
		Namespaces: []*applicationpb.TxNamespace{ns},
		Metadata:   metadata,
	}
}
