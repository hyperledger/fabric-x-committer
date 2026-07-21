/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package workload

import (
	"cmp"

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

// Map maps an array to a new array of the same size using a transformation function.
func Map[T, K any](arr []T, mapper func(index int, value T) K) []K {
	ret := make([]K, len(arr))
	for i, v := range arr {
		ret[i] = mapper(i, v)
	}
	return ret
}
