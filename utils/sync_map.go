/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package utils

import (
	"iter"
	"sync"
)

// SyncMap is a wrapper for sync.Map.
// It enhances readability by explicitly specifying the key and value types at compile time.
// This also minimizes boilerplate code needed for type casting.
type SyncMap[K, V any] struct {
	m sync.Map
}

// Store mirrors sync.Map's method.
// See [sync.Map] for detailed documentation.
func (m *SyncMap[K, V]) Store(key K, value V) {
	m.m.Store(key, value)
}

// Load mirrors sync.Map's method.
// See [sync.Map] for detailed documentation.
func (m *SyncMap[K, V]) Load(key K) (value V, ok bool) {
	return makeReturn[V](m.m.Load(key))
}

// Swap mirrors sync.Map's method.
// See [sync.Map] for detailed documentation.
func (m *SyncMap[K, V]) Swap(key K, value V) (previous V, loaded bool) {
	return makeReturn[V](m.m.Swap(key, value))
}

// LoadOrStore mirrors sync.Map's method.
// See [sync.Map] for detailed documentation.
func (m *SyncMap[K, V]) LoadOrStore(key K, value V) (actual V, loaded bool) {
	return makeReturn[V](m.m.LoadOrStore(key, value))
}

// LoadAndDelete mirrors sync.Map's method.
// See [sync.Map] for detailed documentation.
func (m *SyncMap[K, V]) LoadAndDelete(key K) (value V, loaded bool) {
	return makeReturn[V](m.m.LoadAndDelete(key))
}

// Delete mirrors sync.Map's method.
// See [sync.Map] for detailed documentation.
func (m *SyncMap[K, V]) Delete(key K) {
	m.m.Delete(key)
}

// CompareAndSwap mirrors sync.Map's method.
// See [sync.Map] for detailed documentation.
func (m *SyncMap[K, V]) CompareAndSwap(key K, oldVal, newVal V) (swapped bool) {
	return m.m.CompareAndSwap(key, oldVal, newVal)
}

// CompareAndDelete mirrors sync.Map's method.
// See [sync.Map] for detailed documentation.
func (m *SyncMap[K, V]) CompareAndDelete(key K, value V) (deleted bool) {
	return m.m.CompareAndDelete(key, value)
}

// Range mirrors sync.Map's method.
// See [sync.Map] for detailed documentation.
func (m *SyncMap[K, V]) Range(f func(key K, value V) bool) {
	m.m.Range(func(k, v any) bool {
		nonNilK, _ := k.(K) //nolint:revive // all keys are of type K.
		nonNilV, _ := v.(V) //nolint:revive // all values are of type V.
		return f(nonNilK, nonNilV)
	})
}

// Clear mirrors sync.Map's method.
// See [sync.Map] for detailed documentation.
func (m *SyncMap[K, V]) Clear() {
	m.m.Clear()
}

// IterItems allows iterating over the items' key value pair.
// Example:
//
//	for k, v := range m.IterItems() {
//	    fmt.Printf("%v: %v\n", k, v)
//	}
func (m *SyncMap[K, V]) IterItems() iter.Seq2[K, V] {
	return func(yield func(K, V) bool) {
		m.Range(yield)
	}
}

// IterKeys allows iterating over the items' keys.
// Example:
//
//	for k := range m.IterKeys() {
//	    fmt.Printf("%v\n", k)
//	}
func (m *SyncMap[K, V]) IterKeys() iter.Seq[K] {
	return func(yield func(K) bool) {
		m.Range(func(key K, _ V) bool {
			return yield(key)
		})
	}
}

// IterValues allows iterating over the items' values.
// Example:
//
//	for v := range m.IterValues() {
//	    fmt.Printf("%v\n", v)
//	}
func (m *SyncMap[K, V]) IterValues() iter.Seq[V] {
	return func(yield func(V) bool) {
		m.Range(func(_ K, value V) bool {
			return yield(value)
		})
	}
}

// Count counts the number of items in the map.
// It iterates over the map, so it is best to avoid it in production.
func (m *SyncMap[K, V]) Count() int {
	count := 0
	m.m.Range(func(_, _ any) bool {
		count++
		return true
	})
	return count
}

func makeReturn[T any](v any, retOK bool) (value T, ok bool) {
	if v != nil {
		value = v.(T) //nolint:errcheck,revive,forcetypeassert // all values are of type T.
	}
	return value, retOK
}
