/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package mock

type fifoCache[T any] struct {
	cache         map[string]T
	evictionQueue []string
	evictionIndex int
}

func newFifoCache[T any](size int) *fifoCache[T] {
	return &fifoCache[T]{
		cache:         make(map[string]T, size),
		evictionQueue: make([]string, size),
		evictionIndex: 0,
	}
}

// addIfNotExist returns true if the entry is unique.
// It employs a FIFO eviction policy.
func (c *fifoCache[T]) addIfNotExist(key string, value T) bool {
	if _, ok := c.get(key); ok {
		return false
	}
	delete(c.cache, c.evictionQueue[c.evictionIndex])
	c.cache[key] = value
	c.evictionQueue[c.evictionIndex] = key
	c.evictionIndex = (c.evictionIndex + 1) % len(c.evictionQueue)
	return true
}

// get returns the value of a key.
// If the key does not exist, it returns false.
func (c *fifoCache[T]) get(key string) (T, bool) {
	v, ok := c.cache[key]
	return v, ok
}
