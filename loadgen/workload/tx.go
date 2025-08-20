/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package workload

import (
	"math/rand"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
	"github.com/hyperledger/fabric-x-committer/api/protoloadgen"
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
		Modify(*protoblocktx.Tx)
	}

	// Key is an alias for byte array.
	Key = []byte
)

// GeneratedNamespaceID for now we're only generating transactions for a single namespace.
const GeneratedNamespaceID = "0"

// newIndependentTxGenerator creates a new valid TX generator given a transaction profile.
func newIndependentTxGenerator(
	rnd *rand.Rand, keys *ByteArrayGenerator, profile *TransactionProfile, modifiers ...Modifier,
) *IndependentTxGenerator {
	txb, err := NewTxBuilderFromPolicy(profile.Policy, rnd)
	utils.Must(err)
	return &IndependentTxGenerator{
		TxBuilder:                txb,
		ReadOnlyKeyGenerator:     multiKeyGenerator(rnd, keys, profile.ReadOnlyCount),
		ReadWriteKeyGenerator:    multiKeyGenerator(rnd, keys, profile.ReadWriteCount),
		BlindWriteKeyGenerator:   multiKeyGenerator(rnd, keys, profile.BlindWriteCount),
		ReadWriteValueGenerator:  valueGenerator(rnd, profile.ReadWriteValueSize),
		BlindWriteValueGenerator: valueGenerator(rnd, profile.BlindWriteValueSize),
		Modifiers:                modifiers,
	}
}

// Next generate a new TX.
func (g *IndependentTxGenerator) Next() *protoloadgen.TX {
	readOnly := g.ReadOnlyKeyGenerator.Next()
	readWrite := g.ReadWriteKeyGenerator.Next()
	blindWriteKey := g.BlindWriteKeyGenerator.Next()

	ns := &protoblocktx.TxNamespace{
		NsId:        GeneratedNamespaceID,
		NsVersion:   0,
		ReadsOnly:   make([]*protoblocktx.Read, len(readOnly)),
		ReadWrites:  make([]*protoblocktx.ReadWrite, len(readWrite)),
		BlindWrites: make([]*protoblocktx.Write, len(blindWriteKey)),
	}

	for i, key := range readOnly {
		ns.ReadsOnly[i] = &protoblocktx.Read{Key: key}
	}

	for i, key := range readWrite {
		ns.ReadWrites[i] = &protoblocktx.ReadWrite{
			Key:   key,
			Value: g.ReadWriteValueGenerator.Next(),
		}
	}

	for i, key := range blindWriteKey {
		ns.BlindWrites[i] = &protoblocktx.Write{
			Key:   key,
			Value: g.BlindWriteValueGenerator.Next(),
		}
	}

	tx := &protoblocktx.Tx{
		Namespaces: []*protoblocktx.TxNamespace{ns},
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
