/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package workload

import (
	"fmt"
	"math/rand"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
)

// Dependency types.
const (
	DependencyReadOnly   = "read"
	DependencyReadWrite  = "read-write"
	DependencyBlindWrite = "write"
)

type (
	// signTxModifier signs transactions according to the conflicts profile.
	signTxModifier struct {
		Signer               *TxSignerVerifier
		invalidSignGenerator *FloatToBooleanGenerator
		invalidSignature     [][]byte
	}

	// dependenciesModifier adds dependencies conflicts according to the conflict profile.
	dependenciesModifier struct {
		keyGenerator    *ByteArrayGenerator
		dependencies    []dependencyDesc
		dependenciesMap map[uint64][]dependency
		index           uint64
	}

	dependencyDesc struct {
		bernoulliGenerator *FloatToIntGenerator
		gapGenerator       *FloatToPositiveIntGenerator
		src                string
		dst                string
	}

	dependency struct {
		key []byte
		src string
		dst string
	}
)

func newSignTxModifier(
	rnd *rand.Rand, signer *TxSignerVerifier, profile *Profile,
) *signTxModifier {
	dist := NewBernoulliDistribution(profile.Conflicts.InvalidSignatures)
	invalidTx := &protoblocktx.Tx{
		Id: "fake",
		Namespaces: []*protoblocktx.TxNamespace{
			{
				NsId:      GeneratedNamespaceID,
				NsVersion: 0,
			},
		},
	}
	signer.Sign(invalidTx)
	return &signTxModifier{
		Signer:               signer,
		invalidSignGenerator: dist.MakeBooleanGenerator(rnd),
		invalidSignature:     invalidTx.Signatures,
	}
}

// Modify signs a transaction.
func (g *signTxModifier) Modify(tx *protoblocktx.Tx) (*protoblocktx.Tx, error) {
	switch {
	case tx.Signatures != nil:
		// We support pre-signed TXs
	case g.invalidSignGenerator.Next():
		tx.Signatures = g.invalidSignature
	default:
		g.Signer.Sign(tx)
	}
	return tx, nil
}

func newTxDependenciesModifier(
	rnd *rand.Rand, profile *Profile,
) *dependenciesModifier {
	return &dependenciesModifier{
		keyGenerator: &ByteArrayGenerator{Size: profile.Key.Size, Rnd: rnd},
		dependencies: Map(profile.Conflicts.Dependencies, func(
			_ int, value DependencyDescription,
		) dependencyDesc {
			return dependencyDesc{
				bernoulliGenerator: &FloatToIntGenerator{FloatGen: &BernoulliGenerator{
					Rnd:         rnd,
					Probability: value.Probability,
				}},
				gapGenerator: value.Gap.MakePositiveIntGenerator(rnd),
				src:          value.Src,
				dst:          value.Dst,
			}
		}),
		dependenciesMap: make(map[uint64][]dependency),
	}
}

// Modify injects dependencies.
func (g *dependenciesModifier) Modify(tx *protoblocktx.Tx) (*protoblocktx.Tx, error) {
	depList, ok := g.dependenciesMap[g.index]
	if ok {
		delete(g.dependenciesMap, g.index)
		for _, d := range depList {
			addKey(tx, d.dst, d.key)
		}
	}

	for _, depDesc := range g.dependencies {
		if depDesc.bernoulliGenerator.Next() != 1 {
			continue
		}

		gap := depDesc.gapGenerator.Next()
		d := dependency{
			key: g.keyGenerator.Next(),
			src: depDesc.src,
			dst: depDesc.dst,
		}
		addKey(tx, d.src, d.key)
		g.dependenciesMap[g.index+gap] = append(g.dependenciesMap[g.index+gap], d)
	}

	g.index++
	return tx, nil
}

func addKey(tx *protoblocktx.Tx, dependencyType string, key []byte) {
	txNs := tx.Namespaces[0]
	switch dependencyType {
	case DependencyReadOnly:
		txNs.ReadsOnly = append(txNs.ReadsOnly, &protoblocktx.Read{Key: key})
	case DependencyReadWrite:
		txNs.ReadWrites = append(txNs.ReadWrites, &protoblocktx.ReadWrite{Key: key})
	case DependencyBlindWrite:
		txNs.BlindWrites = append(txNs.BlindWrites, &protoblocktx.Write{Key: key})
	default:
		panic(fmt.Sprintf("invalid dependency type: %s", dependencyType))
	}
}
