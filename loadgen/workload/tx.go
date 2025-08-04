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
		TxBuilderFactory         *TxBuilderFactory
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
		Modify(*TxBuilder) (*TxBuilder, error)
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
	factory, err := NewTxBuilderFactory(profile.Policy, rnd)
	utils.Must(err)
	return &IndependentTxGenerator{
		TxBuilderFactory:         factory,
		ReadOnlyKeyGenerator:     multiKeyGenerator(rnd, keys, profile.ReadOnlyCount),
		ReadWriteKeyGenerator:    multiKeyGenerator(rnd, keys, profile.ReadWriteCount),
		BlindWriteKeyGenerator:   multiKeyGenerator(rnd, keys, profile.BlindWriteCount),
		ReadWriteValueGenerator:  valueGenerator(rnd, profile.ReadWriteValueSize),
		BlindWriteValueGenerator: valueGenerator(rnd, profile.BlindWriteValueSize),
	}
}

// Next generate a new TX.
func (g *IndependentTxGenerator) Next() *TxBuilder {
	txb, err := g.TxBuilderFactory.New()
	utils.Must(err)

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
	txb.Tx.Namespaces = []*protoblocktx.TxNamespace{ns}

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

	return txb
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
func (g *txModifierDecorator) Next() *protoloadgen.TX {
	txb := g.txGen.Next()
	var err error
	for _, mod := range g.modifiers {
		txb, err = mod.Modify(txb)
		if err != nil {
			logger.Infof("Failed modifiying TX with error: %s", err)
		}
	}
	tx, err := txb.Make()
	utils.Must(err)
	return tx
}
