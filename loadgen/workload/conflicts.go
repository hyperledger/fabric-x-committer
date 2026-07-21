/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package workload

import (
	"fmt"
	"math/rand"

	"github.com/hyperledger/fabric-x-common/api/applicationpb"

	"github.com/hyperledger/fabric-x-committer/utils/testsig"
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
		invalidSignGenerator *bernoulliGenerator
		invalidSignature     []byte
	}

	// dependenciesModifier adds dependencies conflicts according to the conflict profile.
	dependenciesModifier struct {
		keyGenerator    *ByteArrayGenerator
		dependencies    []dependencyDesc
		dependenciesMap map[uint64][]dependency
		index           uint64
	}

	dependencyDesc struct {
		trigger *bernoulliGenerator
		gap     uint64
		src     string
		dst     string
	}

	dependency struct {
		key []byte
		src string
		dst string
	}
)

func newSignTxModifier(rnd *rand.Rand, profile *Profile) *signTxModifier {
	return &signTxModifier{
		invalidSignGenerator: &bernoulliGenerator{rnd: rnd, probability: profile.Conflicts.InvalidSignatures},
		invalidSignature:     []byte("dummy"),
	}
}

// Modify signs a transaction.
func (g *signTxModifier) Modify(tx *applicationpb.Tx) {
	if g.invalidSignGenerator.Next() {
		// Pre-assigning prevents TxBuilder from re-signing the TX.
		tx.Endorsements = make([]*applicationpb.Endorsements, len(tx.Namespaces))
		for i := range len(tx.Namespaces) {
			tx.Endorsements[i] = testsig.CreateEndorsementsForThresholdRule(g.invalidSignature)[0]
		}
	}
}

func newTxDependenciesModifier(
	rnd *rand.Rand, profile *Profile,
) *dependenciesModifier {
	return &dependenciesModifier{
		keyGenerator: &ByteArrayGenerator{Size: profile.Key.Size, Source: rnd},
		dependencies: Map(profile.Conflicts.Dependencies, func(
			_ int, value DependencyDescription,
		) dependencyDesc {
			return dependencyDesc{
				trigger: &bernoulliGenerator{rnd: rnd, probability: value.Probability},
				gap:     max(value.Gap, 1),
				src:     value.Src,
				dst:     value.Dst,
			}
		}),
		dependenciesMap: make(map[uint64][]dependency),
	}
}

// Modify injects dependencies.
func (g *dependenciesModifier) Modify(tx *applicationpb.Tx) {
	depList, ok := g.dependenciesMap[g.index]
	if ok {
		delete(g.dependenciesMap, g.index)
		for _, d := range depList {
			addKey(tx, d.dst, d.key)
		}
	}

	for _, depDesc := range g.dependencies {
		if !depDesc.trigger.Next() {
			continue
		}

		d := dependency{
			key: g.keyGenerator.Next(),
			src: depDesc.src,
			dst: depDesc.dst,
		}
		addKey(tx, d.src, d.key)
		g.dependenciesMap[g.index+depDesc.gap] = append(g.dependenciesMap[g.index+depDesc.gap], d)
	}

	g.index++
}

func addKey(tx *applicationpb.Tx, dependencyType string, key []byte) {
	txNs := tx.Namespaces[0]
	switch dependencyType {
	case DependencyReadOnly:
		txNs.ReadsOnly = append(txNs.ReadsOnly, &applicationpb.Read{Key: key})
	case DependencyReadWrite:
		txNs.ReadWrites = append(txNs.ReadWrites, &applicationpb.ReadWrite{Key: key})
	case DependencyBlindWrite:
		txNs.BlindWrites = append(txNs.BlindWrites, &applicationpb.Write{Key: key})
	default:
		panic(fmt.Sprintf("invalid dependency type: %s", dependencyType))
	}
}

// bernoulliGenerator yields true with the configured probability.
type bernoulliGenerator struct {
	rnd         *rand.Rand
	probability Probability
}

// Next yields true with the configured probability.
func (g *bernoulliGenerator) Next() bool {
	return g.rnd.Float64() < g.probability
}
