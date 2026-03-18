/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"bytes"
	"sync"
)

// SafeBuffer is a thread-safe wrapper around bytes.Buffer.
// It protects concurrent access to the underlying buffer using a mutex,
// preventing race conditions when multiple goroutines read from or write to
// the same buffer simultaneously.
//
// This is particularly useful in testing scenarios where command output
// is being written by one goroutine while another goroutine is reading
// or checking the buffer contents.
type SafeBuffer struct {
	b bytes.Buffer
	m sync.Mutex
}

// Write appends the contents of p to the buffer in a thread-safe manner.
// It implements the io.Writer interface.
// Returns the number of bytes written and any error encountered.
func (sb *SafeBuffer) Write(p []byte) (n int, err error) {
	sb.m.Lock()
	defer sb.m.Unlock()
	return sb.b.Write(p)
}

// Read reads the next len(p) bytes from the buffer in a thread-safe manner.
// It implements the io.Reader interface.
// Returns the number of bytes read and any error encountered.
func (sb *SafeBuffer) Read(p []byte) (n int, err error) {
	sb.m.Lock()
	defer sb.m.Unlock()
	return sb.b.Read(p)
}

// String returns the contents of the buffer as a string in a thread-safe manner.
// This is safe to call concurrently with Write operations.
func (sb *SafeBuffer) String() string {
	sb.m.Lock()
	defer sb.m.Unlock()
	return sb.b.String()
}

// Reset resets the buffer to be empty in a thread-safe manner.
// This is useful for reusing the same SafeBuffer across multiple test cases.
func (sb *SafeBuffer) Reset() {
	sb.m.Lock()
	defer sb.m.Unlock()
	sb.b.Reset()
}

// Len returns the number of bytes in the buffer in a thread-safe manner.
func (sb *SafeBuffer) Len() int {
	sb.m.Lock()
	defer sb.m.Unlock()
	return sb.b.Len()
}
