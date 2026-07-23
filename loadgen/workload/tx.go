/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package workload

import (
	"math/rand"

	"github.com/hyperledger/fabric-x-common/api/applicationpb"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/utils"
	"github.com/hyperledger/fabric-x-committer/utils/testsig"
)

// invalidSignatureBytes is the dummy endorsement stamped on transactions selected to be invalid.
var invalidSignatureBytes = []byte("dummy")

type (
	// IndependentTxGenerator generates a new valid TX given key generators. With a non-zero
	// invalid-signature probability it may instead stamp a dummy endorsement, decided from its own
	// per-worker random source so the decision is independent of the generated content.
	IndependentTxGenerator struct {
		TxBuilder                *TxBuilder
		ReadOnlyKeyGenerator     *MultiGenerator[Key]
		ReadWriteKeyGenerator    *MultiGenerator[Key]
		BlindWriteKeyGenerator   *MultiGenerator[Key]
		ReadWriteValueGenerator  *ByteArrayGenerator
		BlindWriteValueGenerator *ByteArrayGenerator
		MetadataGenerator        *ByteArrayGenerator
		Modifiers                []Modifier
		invalidSignRnd           *rand.Rand
		invalidSignProbability   Probability
	}

	// Modifier modifies a TX.
	Modifier interface {
		Modify(*applicationpb.Tx)
	}

	// Key is an alias for byte array.
	Key = []byte
)

// DefaultGeneratedNamespaceID for now we're only generating transactions for a single namespace.
const DefaultGeneratedNamespaceID = "0"

// newIndependentTxGenerators creates workers that generate independent transactions and apply the
// modifiers. Each worker will have a unique instance of the modifier to avoid concurrency issues.
// The modifiers are applied in the order they are given; a modifier can modify any of the TX fields
// to adjust the workload (for example, adding dependencies). The invalid-signature stamp is applied
// last, after the modifiers, so it overrides any endorsement they may have produced.
func newIndependentTxGenerators(profile *Profile, extraModifiers ...Generator[Modifier]) []*IndependentTxGenerator {
	seeders, keyGens := newSeedersAndKeyGens(profile)
	gens := make([]*IndependentTxGenerator, len(seeders))
	for i, s := range seeders {
		modifiers := make([]Modifier, 0, len(extraModifiers)+1)
		if len(profile.Transaction.Dependencies) > 0 {
			modifiers = append(modifiers, newTxDependenciesModifier(s.nextSeed(), profile))
		}
		for _, mod := range extraModifiers {
			modifiers = append(modifiers, mod.Next())
		}
		invalidSignRnd := s.nextSeed()
		txb, err := NewTxBuilderFromPolicy(&profile.Policy, s.nextSeed())
		utils.Must(err)
		gens[i] = &IndependentTxGenerator{
			TxBuilder:                txb,
			ReadOnlyKeyGenerator:     multiKeyGenerator(keyGens[i], profile.Transaction.ReadOnlyCount),
			ReadWriteKeyGenerator:    multiKeyGenerator(keyGens[i], profile.Transaction.ReadWriteCount),
			BlindWriteKeyGenerator:   multiKeyGenerator(keyGens[i], profile.Transaction.BlindWriteCount),
			ReadWriteValueGenerator:  valueGenerator(s.nextSeed(), profile.Transaction.ReadWriteValueSize),
			BlindWriteValueGenerator: valueGenerator(s.nextSeed(), profile.Transaction.BlindWriteValueSize),
			MetadataGenerator:        valueGenerator(s.nextSeed(), profile.Transaction.MetadataSize),
			Modifiers:                modifiers,
			invalidSignRnd:           invalidSignRnd,
			invalidSignProbability:   profile.Transaction.InvalidSignatures,
		}
	}
	return gens
}

// Next generate a new TX.
func (g *IndependentTxGenerator) Next() *servicepb.LoadGenTx {
	readOnly := g.ReadOnlyKeyGenerator.Next()
	readWrite := g.ReadWriteKeyGenerator.Next()
	blindWriteKey := g.BlindWriteKeyGenerator.Next()

	ns := &applicationpb.TxNamespace{
		NsId:        DefaultGeneratedNamespaceID,
		NsVersion:   0,
		ReadsOnly:   make([]*applicationpb.Read, len(readOnly)),
		ReadWrites:  make([]*applicationpb.ReadWrite, len(readWrite)),
		BlindWrites: make([]*applicationpb.Write, len(blindWriteKey)),
	}

	for i, key := range readOnly {
		ns.ReadsOnly[i] = &applicationpb.Read{Key: key}
	}

	for i, key := range readWrite {
		ns.ReadWrites[i] = &applicationpb.ReadWrite{
			Key:   key,
			Value: g.ReadWriteValueGenerator.Next(),
		}
	}

	for i, key := range blindWriteKey {
		ns.BlindWrites[i] = &applicationpb.Write{
			Key:   key,
			Value: g.BlindWriteValueGenerator.Next(),
		}
	}

	// We include a metadata only if specifically requested.
	var metadata [][]byte
	metadataItem := g.MetadataGenerator.Next()
	if len(metadataItem) > 0 {
		metadata = [][]byte{metadataItem}
	}

	tx := &applicationpb.Tx{
		Namespaces: []*applicationpb.TxNamespace{ns},
		Metadata:   metadata,
	}
	for _, mod := range g.Modifiers {
		mod.Modify(tx)
	}
	if g.invalidSignRnd.Float64() < g.invalidSignProbability {
		// Pre-assigning a dummy endorsement prevents TxBuilder from producing a valid signature.
		tx.Endorsements = make([]*applicationpb.Endorsements, len(tx.Namespaces))
		for i := range tx.Namespaces {
			tx.Endorsements[i] = testsig.CreateEndorsementsForThresholdRule(invalidSignatureBytes)[0]
		}
	}
	return g.TxBuilder.MakeTx(tx)
}

func multiKeyGenerator(keyGen Generator[Key], keyCount uint32) *MultiGenerator[Key] {
	return &MultiGenerator[Key]{
		Gen:   keyGen,
		Count: int(keyCount),
	}
}

func valueGenerator(rnd *rand.Rand, valueSize uint32) *ByteArrayGenerator {
	return &ByteArrayGenerator{Size: valueSize, Source: rnd}
}
