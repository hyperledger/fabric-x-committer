/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package workload

import "math/rand"

type (
	seeder struct {
		seed *rand.Rand
	}
	seederWithKeys struct {
		seeder
		keyGen *ByteArrayGenerator
	}
)

func newSeedersWithKeys(profile *Profile) []seederWithKeys {
	s := seeder{seed: rand.New(rand.NewSource(profile.Seed))}
	seeders := make([]seederWithKeys, profile.Workers)
	for i := range seeders {
		// Each worker has a unique seed to generate keys in addition the seed for the other content.
		// This allows reproducing the generated keys regardless of the other generated content.
		// It is useful when generating transactions, and later generating queries for the same keys.
		workerSg := seeder{seed: s.nextSeed()}
		seeders[i] = seederWithKeys{
			seeder: workerSg,
			keyGen: &ByteArrayGenerator{Size: profile.Key.Size, Source: workerSg.nextSeed()},
		}
	}
	return seeders
}

func (w *seeder) nextSeed() *rand.Rand {
	return rand.New(rand.NewSource(w.seed.Int63()))
}
