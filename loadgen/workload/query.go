/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package workload

import (
	"math"
	"math/rand"

	"github.com/hyperledger/fabric-x-common/api/committerpb"
)

// QueryGenerator generates a new query for keys.
type QueryGenerator struct {
	ValidKeyGenerator   *ByteArrayGenerator
	InvalidKeyGenerator *ByteArrayGenerator
	Size                Generator[int]
	InvalidPortion      Generator[float64]
	ShuffleRnd          *rand.Rand
	Shuffle             bool
}

// newIndependentQueryGenerators creates workers that generates independent queries.
func newIndependentQueryGenerators(profile *Profile) []*QueryGenerator {
	seeders, keyGens := newSeedersAndKeyGens(profile)
	gens := make([]*QueryGenerator, len(seeders))
	for i, s := range seeders {
		gens[i] = &QueryGenerator{
			ValidKeyGenerator:   keyGens[i],
			InvalidKeyGenerator: &ByteArrayGenerator{Size: profile.Key.Size, Source: s.nextSeed()},
			Size:                profile.Query.QuerySize.MakeIntGenerator(s.nextSeed()),
			InvalidPortion:      profile.Query.MinInvalidKeysPortion.MakeGenerator(s.nextSeed()),
			ShuffleRnd:          s.nextSeed(),
			Shuffle:             profile.Query.Shuffle,
		}
	}
	return gens
}

// Next generate a new query.
func (g *QueryGenerator) Next() *committerpb.Query {
	size := g.Size.Next()
	keys := make([][]byte, 0, size)

	invalidSize := Clip(int(math.Round(g.InvalidPortion.Next()*float64(size))), 0, size)
	keys = append(keys, GenerateArray(g.ValidKeyGenerator, size-invalidSize)...)
	keys = append(keys, GenerateArray(g.InvalidKeyGenerator, invalidSize)...)

	if g.Shuffle {
		g.ShuffleRnd.Shuffle(size, func(i, j int) {
			keys[i], keys[j] = keys[j], keys[i]
		})
	}

	return &committerpb.Query{
		Namespaces: []*committerpb.QueryNamespace{
			{
				NsId: DefaultGeneratedNamespaceID,
				Keys: keys,
			},
		},
	}
}
