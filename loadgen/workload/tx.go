/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package workload

import (
	"math/rand"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
)

type (
	// IndependentTxGenerator generates a new valid TX given key generators.
	IndependentTxGenerator struct {
		TxIDGenerator            *UUIDGenerator
		ReadOnlyKeyGenerator     *MultiGenerator[Key]
		ReadWriteKeyGenerator    *MultiGenerator[Key]
		BlindWriteKeyGenerator   *MultiGenerator[Key]
		ReadWriteValueGenerator  *ByteArrayGenerator
		BlindWriteValueGenerator *ByteArrayGenerator
	}

	txModifierDecorator struct {
		txGen     *IndependentTxGenerator
		modifiers []Modifier
	}

	// Modifier modifies a TX.
	Modifier interface {
		Modify(*protoblocktx.Tx) (*protoblocktx.Tx, error)
	}

	// Key is an alias for byte array.
	Key = []byte
)

// GeneratedNamespaceID for now we're only generating transactions for a single namespace.
const GeneratedNamespaceID = "0"

// newIndependentTxGenerator creates a new valid TX generator given a transaction profile.
func newIndependentTxGenerator(
	rnd *rand.Rand, keys *ByteArrayGenerator, profile *TransactionProfile,
) *IndependentTxGenerator {
	return &IndependentTxGenerator{
		TxIDGenerator:            &UUIDGenerator{Rnd: rnd},
		ReadOnlyKeyGenerator:     multiKeyGenerator(rnd, keys, profile.ReadOnlyCount),
		ReadWriteKeyGenerator:    multiKeyGenerator(rnd, keys, profile.ReadWriteCount),
		BlindWriteKeyGenerator:   multiKeyGenerator(rnd, keys, profile.BlindWriteCount),
		ReadWriteValueGenerator:  valueGenerator(rnd, profile.ReadWriteValueSize),
		BlindWriteValueGenerator: valueGenerator(rnd, profile.BlindWriteValueSize),
	}
}

// Next generate a new TX.
func (g *IndependentTxGenerator) Next() *protoblocktx.Tx {
	readOnly := g.ReadOnlyKeyGenerator.Next()
	readWrite := g.ReadWriteKeyGenerator.Next()
	blindWriteKey := g.BlindWriteKeyGenerator.Next()

	tx := &protoblocktx.Tx{
		Id: g.TxIDGenerator.Next(),
		Namespaces: []*protoblocktx.TxNamespace{
			{
				NsId:        GeneratedNamespaceID,
				NsVersion:   0,
				ReadsOnly:   make([]*protoblocktx.Read, len(readOnly)),
				ReadWrites:  make([]*protoblocktx.ReadWrite, len(readWrite)),
				BlindWrites: make([]*protoblocktx.Write, len(blindWriteKey)),
			},
		},
	}

	for i, key := range readOnly {
		tx.Namespaces[0].ReadsOnly[i] = &protoblocktx.Read{Key: key}
	}

	for i, key := range readWrite {
		tx.Namespaces[0].ReadWrites[i] = &protoblocktx.ReadWrite{
			Key:   key,
			Value: g.ReadWriteValueGenerator.Next(),
		}
	}

	for i, key := range blindWriteKey {
		tx.Namespaces[0].BlindWrites[i] = &protoblocktx.Write{
			Key:   key,
			Value: g.BlindWriteValueGenerator.Next(),
		}
	}

	return tx
}

func multiKeyGenerator(rnd *rand.Rand, keyGen Generator[Key], keyCount *Distribution) *MultiGenerator[Key] {
	return &MultiGenerator[Key]{
		Gen:   keyGen,
		Count: keyCount.MakeIntGenerator(rnd),
	}
}

func valueGenerator(rnd *rand.Rand, valueSize uint32) *ByteArrayGenerator {
	return &ByteArrayGenerator{Size: valueSize, Rnd: rnd}
}

// newTxModifierTxDecorator wraps a TX generator and apply one or more modification methods.
func newTxModifierTxDecorator(txGen *IndependentTxGenerator, modifiers ...Modifier) *txModifierDecorator {
	return &txModifierDecorator{
		txGen:     txGen,
		modifiers: modifiers,
	}
}

// Next apply all the modifiers to a transaction.
func (g *txModifierDecorator) Next() *protoblocktx.Tx {
	tx := g.txGen.Next()
	var err error
	for _, mod := range g.modifiers {
		tx, err = mod.Modify(tx)
		if err != nil {
			logger.Infof("Failed modifiying TX with error: %s", err)
		}
	}
	return tx
}
