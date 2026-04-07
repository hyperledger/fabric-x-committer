/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package workload

import (
	"math/rand"
)

// Probability is a float in the closed interval [0,1].
type Probability = float64

const (
	// Always is 100%.
	Always Probability = 1
	// Never is 0%.
	Never Probability = 0
)

type (
	// Distribution descriptor for the available distributions.
	Distribution struct {
		Const     float64                  `mapstructure:"const" yaml:"const,omitempty"`
		Uniform   *UniformDist             `mapstructure:"uniform" yaml:"uniform,omitempty"`
		Normal    *NormalDist              `mapstructure:"normal" yaml:"normal,omitempty"`
		Bernoulli Probability              `mapstructure:"bernoulli" yaml:"bernoulli,omitempty"`
		Discrete  []DiscreteValue[float64] `mapstructure:"discrete" yaml:"discrete,omitempty"`
	}
	// UniformDist describes uniform.
	UniformDist struct {
		Min float64 `mapstructure:"min" yaml:"min,omitempty"`
		Max float64 `mapstructure:"max" yaml:"max,omitempty"`
	}
	// NormalDist describes normal.
	NormalDist struct {
		Mean float64 `mapstructure:"mean" yaml:"mean,omitempty"`
		Std  float64 `mapstructure:"std" yaml:"std,omitempty"`
	}
)

// MakeGenerator returns a new generator according to the distribution description.
// The default distribution is const=0.
func (d *Distribution) MakeGenerator(rnd *rand.Rand) Generator[float64] {
	if d == nil {
		return &ConstGenerator[float64]{Const: 0}
	}
	switch {
	case d.Uniform != nil:
		return &uniformGenerator{
			Rnd: rnd,
			Min: d.Uniform.Min,
			Max: d.Uniform.Max,
		}
	case d.Normal != nil:
		return &normalGenerator{
			Rnd:  rnd,
			Mean: d.Normal.Mean,
			Std:  d.Normal.Std,
		}
	case d.Discrete != nil:
		return &DiscreteGenerator[float64]{
			Rnd:    rnd,
			Values: d.Discrete,
		}
	case d.Bernoulli > 0:
		return &BernoulliGenerator{
			Rnd:         rnd,
			Probability: d.Bernoulli,
		}
	default:
		return &ConstGenerator[float64]{Const: d.Const}
	}
}

// MakeIntGenerator returns a new integer generator according to the distribution description.
func (d *Distribution) MakeIntGenerator(rnd *rand.Rand) *FloatToIntGenerator {
	return &FloatToIntGenerator{FloatGen: d.MakeGenerator(rnd)}
}

// MakePositiveIntGenerator returns a new positive (>=1) integer generator according to the distribution description.
func (d *Distribution) MakePositiveIntGenerator(rnd *rand.Rand) *FloatToPositiveIntGenerator {
	return &FloatToPositiveIntGenerator{FloatGen: d.MakeGenerator(rnd)}
}

// MakeBooleanGenerator returns a new boolean generator according to the distribution description.
func (d *Distribution) MakeBooleanGenerator(rnd *rand.Rand) *FloatToBooleanGenerator {
	return &FloatToBooleanGenerator{FloatGen: d.MakeGenerator(rnd)}
}

// NewConstantDistribution creates a constant value distribution.
func NewConstantDistribution(value float64) *Distribution {
	return &Distribution{Const: value}
}

// NewNormalDistribution creates a normal distribution.
func NewNormalDistribution(mean, std float64) *Distribution {
	return &Distribution{
		Normal: &NormalDist{
			Mean: mean,
			Std:  std,
		},
	}
}

// NewUniformDistribution creates a uniform distribution.
func NewUniformDistribution(minVal, maxVal float64) *Distribution {
	return &Distribution{
		Uniform: &UniformDist{
			Min: minVal,
			Max: maxVal,
		},
	}
}

// NewDiscreteDistribution creates a discrete distribution.
func NewDiscreteDistribution(values []DiscreteValue[float64]) *Distribution {
	return &Distribution{Discrete: values}
}

// NewBernoulliDistribution creates a Bernoulli distribution.
func NewBernoulliDistribution(probability Probability) *Distribution {
	return &Distribution{Bernoulli: probability}
}

// uniformGenerator generates values with a uniform distribution.
type uniformGenerator struct {
	Rnd      *rand.Rand
	Min, Max float64
}

// Next yields the next uniform value.
func (d *uniformGenerator) Next() float64 {
	return d.Rnd.Float64()*(d.Max-d.Min) + d.Min
}

// normalGenerator generates values with a normal distribution.
type normalGenerator struct {
	Rnd       *rand.Rand
	Mean, Std float64
}

// Next yields the next normal value.
func (d *normalGenerator) Next() float64 {
	return d.Rnd.NormFloat64()*d.Std + d.Mean
}

// BernoulliGenerator generates 1 with probability of p.
type BernoulliGenerator struct {
	Rnd         *rand.Rand
	Probability Probability
}

// Next yields the next bernoulli value.
func (d *BernoulliGenerator) Next() float64 {
	if d.Rnd.Float64() < d.Probability {
		return 1
	}
	return 0
}

// DiscreteValue describe the appearance probability of a value.
type DiscreteValue[T any] struct {
	Value       T           `mapstructure:"value"`
	Probability Probability `mapstructure:"probability"`
}

// DiscreteGenerator generates values with a discrete distribution.
type DiscreteGenerator[T any] struct {
	Rnd    *rand.Rand
	Values []DiscreteValue[T]
}

// Next yields the next discrete value.
func (d *DiscreteGenerator[T]) Next() T {
	r := d.Rnd.Float64()
	remaining := float64(1)
	for _, value := range d.Values {
		remaining -= value.Probability
		if r >= remaining {
			return value.Value
		}
	}
	panic("probabilities must sum to 1")
}
