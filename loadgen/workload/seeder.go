/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package workload

import "math/rand"

type seeder struct {
	seed *rand.Rand
}

// newSeedersAndKeyGens creates a list of seeder and key generators (1 for each worker).
// Each worker has a unique seed to generate keys in addition the seed for the other content.
// This allows reproducing the generated keys regardless of the other generated content.
// It is useful when generating transactions, and later generating queries for the same keys.
func newSeedersAndKeyGens(profile *Profile) ([]seeder, []*ByteArrayGenerator) {
	s := seeder{seed: rand.New(rand.NewSource(profile.Seed))}
	keyGens := make([]*ByteArrayGenerator, profile.Workers)
	for i := range profile.Workers {
		keyGens[i] = &ByteArrayGenerator{Size: profile.Key.Size, Source: s.nextSeed()}
	}
	seeders := make([]seeder, profile.Workers)
	for i := range profile.Workers {
		seeders[i].seed = s.nextSeed()
	}
	return seeders, keyGens
}

func (w *seeder) nextSeed() *rand.Rand {
	return rand.New(rand.NewSource(w.seed.Int63()))
}
