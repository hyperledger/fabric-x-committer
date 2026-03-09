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
)

type (
	// IndependentTxGenerator generates a new valid TX given key generators.
	IndependentTxGenerator struct {
		TxBuilder                *TxBuilder
		ReadOnlyKeyGenerator     *MultiGenerator[Key]
		ReadWriteKeyGenerator    *MultiGenerator[Key]
		BlindWriteKeyGenerator   *MultiGenerator[Key]
		ReadWriteValueGenerator  *ByteArrayGenerator
		BlindWriteValueGenerator *ByteArrayGenerator
		Modifiers                []Modifier
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

// newIndependentTxGenerators creates workers that generates independent transactions and apply the modifiers.
// Each worker will have a unique instance of the modifier to avoid concurrency issues.
// The modifiers will be applied in the order they are given.
// A transaction modifier can modify any of its fields to adjust the workload.
// For example, a modifier can query the database for the read-set versions to simulate a real transaction.
// The signature modifier is applied last so all previous modifications will be signed correctly.
func newIndependentTxGenerators(profile *Profile, extraModifiers ...Generator[Modifier]) []*IndependentTxGenerator {
	seeders, keyGens := newSeedersAndKeyGens(profile)
	gens := make([]*IndependentTxGenerator, len(seeders))
	for i, s := range seeders {
		modifiers := make([]Modifier, 0, len(extraModifiers)+2)
		if len(profile.Conflicts.Dependencies) > 0 {
			modifiers = append(modifiers, newTxDependenciesModifier(s.nextSeed(), profile))
		}
		for _, mod := range extraModifiers {
			modifiers = append(modifiers, mod.Next())
		}
		modifiers = append(modifiers, newSignTxModifier(s.nextSeed(), profile))
		txb, err := NewTxBuilderFromPolicy(&profile.Policy, s.nextSeed())
		utils.Must(err)
		gens[i] = &IndependentTxGenerator{
			TxBuilder:                txb,
			ReadOnlyKeyGenerator:     multiKeyGenerator(s.nextSeed(), keyGens[i], profile.Transaction.ReadOnlyCount),
			ReadWriteKeyGenerator:    multiKeyGenerator(s.nextSeed(), keyGens[i], profile.Transaction.ReadWriteCount),
			BlindWriteKeyGenerator:   multiKeyGenerator(s.nextSeed(), keyGens[i], profile.Transaction.BlindWriteCount),
			ReadWriteValueGenerator:  valueGenerator(s.nextSeed(), profile.Transaction.ReadWriteValueSize),
			BlindWriteValueGenerator: valueGenerator(s.nextSeed(), profile.Transaction.BlindWriteValueSize),
			Modifiers:                modifiers,
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

	tx := &applicationpb.Tx{
		Namespaces: []*applicationpb.TxNamespace{ns},
	}
	for _, mod := range g.Modifiers {
		mod.Modify(tx)
	}
	return g.TxBuilder.MakeTx(tx)
}

func multiKeyGenerator(rnd *rand.Rand, keyGen Generator[Key], keyCount *Distribution) *MultiGenerator[Key] {
	return &MultiGenerator[Key]{
		Gen:   keyGen,
		Count: keyCount.MakeIntGenerator(rnd),
	}
}

func valueGenerator(rnd *rand.Rand, valueSize uint32) *ByteArrayGenerator {
	return &ByteArrayGenerator{Size: valueSize, Source: rnd}
}
