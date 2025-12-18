/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package workload

import (
	"math"
	"math/rand"

	"github.com/hyperledger/fabric-x-committer/api/committerpb"
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

// newIndependentTxGenerator creates a new QueryGenerator.
func newQueryGenerator(rnd *rand.Rand, keys *ByteArrayGenerator, profile *Profile) *QueryGenerator {
	return &QueryGenerator{
		ValidKeyGenerator:   keys,
		InvalidKeyGenerator: &ByteArrayGenerator{Size: profile.Key.Size, Source: rnd},
		Size:                profile.Query.QuerySize.MakeIntGenerator(rnd),
		InvalidPortion:      profile.Query.MinInvalidKeysPortion.MakeGenerator(rnd),
		ShuffleRnd:          rnd,
		Shuffle:             profile.Query.Shuffle,
	}
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
