/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package workload

import (
	"fmt"
	"math"
	"math/rand"
	"slices"
	"testing"

	"github.com/mitchellh/mapstructure"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"
	"golang.org/x/exp/constraints"
)

func makeRand() *rand.Rand {
	return rand.New(rand.NewSource(0))
}

func requireMarshalUnmarshal(t *testing.T, expected string, in *Distribution) {
	t.Helper()
	values := make(map[string]any)
	require.NoError(t, yaml.Unmarshal([]byte(expected), &values))
	out := &Distribution{}
	require.NoError(t, mapstructure.Decode(values, out))
	fmt.Println(out)

	rnd := makeRand()
	assert.Equal(t, in.MakeGenerator(rnd), out.MakeGenerator(rnd))
	assert.Equal(t, in.MakeIntGenerator(rnd), out.MakeIntGenerator(rnd))
	assert.Equal(t, in.MakePositiveIntGenerator(rnd), out.MakePositiveIntGenerator(rnd))
}

func requireAllInDelta[T any](t *testing.T, arr []T, expected T, delta float64) {
	t.Helper()
	for _, v := range arr {
		require.InDelta(t, expected, v, delta)
	}
}

func requireAllEqual[T any](t *testing.T, arr []T, expected T) {
	t.Helper()
	for _, v := range arr {
		require.Equal(t, expected, v)
	}
}

func TestConstDist(t *testing.T) {
	t.Parallel()

	t.Run("marshal", func(t *testing.T) {
		t.Parallel()
		requireMarshalUnmarshal(t, "const: 0\ntype: constant\n", NewConstantDistribution(0))
	})

	for _, val := range []int{-1, 0, 1, 2, 10} {
		val := val
		t.Run(fmt.Sprintf("generate:%d", val), func(t *testing.T) {
			t.Parallel()
			d := NewConstantDistribution(float64(val))
			requireAllInDelta(t, GenerateArray(d.MakeGenerator(makeRand()), int(1e3)), float64(val), 1e-10)
			requireAllEqual(t, GenerateArray(d.MakeIntGenerator(makeRand()), int(1e3)), val)
			//nolint:gosec // integer overflow conversion int -> uint64.
			requireAllEqual(t, GenerateArray(d.MakePositiveIntGenerator(makeRand()), 1e3), uint64(max(val, 1)))
		})
	}
}

func TestNormDist(t *testing.T) {
	t.Parallel()

	t.Run("marshal", func(t *testing.T) {
		t.Parallel()
		requireMarshalUnmarshal(t, "normal:\n  mean: 0\n  std: 1\n", NewNormalDistribution(0, 1))
	})

	t.Run("generate", func(t *testing.T) {
		t.Parallel()
		g := NewNormalDistribution(0, 1).MakeGenerator(makeRand())
		values := GenerateArray(g, int(1e6))
		mean, std := MeanStd(values)
		require.InDelta(t, 0, mean, 1e-3)
		require.InDelta(t, 1, std, 1e-3)
	})
}

func TestUniformDist(t *testing.T) {
	t.Parallel()

	t.Run("marshal", func(t *testing.T) {
		t.Parallel()
		requireMarshalUnmarshal(t, "uniform:\n  max: 1\n  min: 0\n", NewUniformDistribution(0, 1))
	})

	t.Run("generate", func(t *testing.T) {
		t.Parallel()
		g := NewUniformDistribution(-1, 1).MakeGenerator(makeRand())
		values := GenerateArray(g, int(1e6))
		requireAllInDelta(t, values, 0, 1)
		mean, std := MeanStd(values)
		require.InDelta(t, 0, mean, 1e-3)
		// uniform std: sqrt( (1/12) * (b-a)^2 )
		require.InDelta(t, math.Sqrt(1./3.), std, 1e-3)
	})
}

func TestBernoulliDist(t *testing.T) {
	t.Parallel()

	t.Run("marshal", func(t *testing.T) {
		t.Parallel()
		requireMarshalUnmarshal(t, "bernoulli: 0.2\n", NewBernoulliDistribution(0.2))
	})

	t.Run("generate", func(t *testing.T) {
		t.Parallel()
		g := NewBernoulliDistribution(0.2).MakeGenerator(makeRand())
		values := GenerateArray(g, int(1e6))
		requireBernoulliDist(t, values, 0.2, 1e-3)
	})
}

func requireDiscreteDist[S, D constraints.Float | constraints.Integer](
	t *testing.T, sample []S, distValues []DiscreteValue[D], delta float64,
) {
	t.Helper()
	options := Map(distValues, func(_ int, v DiscreteValue[D]) int { return int(v.Value) })
	buckets := make([]uint64, len(options))
	for _, v := range sample {
		i := slices.Index(options, int(v))
		require.GreaterOrEqualf(t, i, 0, "invalid value: %v", v)
		buckets[i]++
	}

	var probabilitySum float64
	for i, v := range distValues {
		expectedProbability := min(v.Probability, 1-probabilitySum)
		require.InDelta(t, expectedProbability, float64(buckets[i])/float64(len(sample)), delta)
		probabilitySum += expectedProbability
	}
}

func requireBernoulliDist(t *testing.T, sample []float64, probability Probability, delta float64) {
	t.Helper()
	requireDiscreteDist(t, sample, []DiscreteValue[float64]{
		{Value: 1, Probability: probability},
		{Value: 0, Probability: Always},
	}, delta)
}

func TestDiscreteDistMarshal(t *testing.T) {
	t.Parallel()

	t.Run("marshal", func(t *testing.T) {
		t.Parallel()
		requireMarshalUnmarshal(t,
			"discrete:\n  - value: 0\n    probability: 1\n",
			NewDiscreteDistribution([]DiscreteValue[float64]{{Value: 0, Probability: 1}}),
		)
	})

	t.Run("generate", func(t *testing.T) {
		t.Parallel()
		distValues := []DiscreteValue[float64]{
			{Value: 0, Probability: 0.1},
			{Value: 1, Probability: 0.2},
			{Value: 2, Probability: Always},
		}
		g := NewDiscreteDistribution(distValues).MakeGenerator(makeRand())

		values := GenerateArray(g, int(1e6))
		requireDiscreteDist(t, values, distValues, 1e-3)
	})
}
