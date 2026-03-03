/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package workload

import (
	"cmp"
	"math"
	"math/rand"

	"golang.org/x/exp/constraints"
)

// Clip returns the given value, clipped between the given boundaries.
func Clip[T cmp.Ordered](value, low, high T) T {
	return min(high, max(low, value))
}

// SumInt returns the sum of an array of integers.
func SumInt[T constraints.Integer](arr []T) int64 {
	var sum int64
	for _, v := range arr {
		sum += int64(v)
	}
	return sum
}

// Sum returns the sum of an array of floats.
func Sum[T constraints.Float](arr []T) float64 {
	var sum float64
	for _, v := range arr {
		sum += float64(v)
	}
	return sum
}

// MeanStd returns the mean and std of an array of float samples.
func MeanStd[T constraints.Float](arr []T) (mean, std float64) {
	n := float64(len(arr))
	mean = Sum(arr) / n

	// Sample std: sqrt( Σ (v - mean)^2 / (n-1) )
	var sqrSum float64
	for _, v := range arr {
		d := float64(v) - mean
		sqrSum += d * d
	}
	std = math.Sqrt(sqrSum / (n - 1))
	return mean, std
}

// Map maps an array to a new array of the same size using a transformation function.
func Map[T, K any](arr []T, mapper func(index int, value T) K) []K {
	ret := make([]K, len(arr))
	for i, v := range arr {
		ret[i] = mapper(i, v)
	}
	return ret
}

// NewRandFromSeedGenerator creates a new random generator using a generated seed.
func NewRandFromSeedGenerator(seedRnd *rand.Rand) *rand.Rand {
	return rand.New(rand.NewSource(seedRnd.Int63()))
}

// Must panics in case of an error.
func Must(err error) {
	if err != nil {
		panic(err)
	}
}
