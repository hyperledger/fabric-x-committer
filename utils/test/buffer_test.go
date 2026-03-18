/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSafeBuffer_BasicOperations(t *testing.T) {
	t.Parallel()

	var sb SafeBuffer
	data := "test data"

	// Test initial state
	require.Empty(t, sb.String())
	require.Equal(t, 0, sb.Len())

	// Test Write
	n, err := sb.Write([]byte(data))
	require.NoError(t, err)
	require.Equal(t, len(data), n)
	require.Equal(t, data, sb.String())
	require.Equal(t, 9, sb.Len())

	// Test Read
	buf := make([]byte, 4)
	n, err = sb.Read(buf)
	require.NoError(t, err)
	require.Equal(t, 4, n)
	require.Equal(t, data[:4], string(buf))

	// Test Reset
	sb.Reset()
	require.Empty(t, sb.String())
	require.Equal(t, 0, sb.Len())

	// Test multiple writes
	_, err = sb.Write([]byte(data[:4]))
	require.NoError(t, err)
	require.Equal(t, data[:4], sb.String())
	require.Equal(t, 4, sb.Len())

	_, err = sb.Write([]byte(data[4:]))
	require.NoError(t, err)
	require.Equal(t, data, sb.String())
	require.Equal(t, 9, sb.Len())
}

// TestSafeBuffer_ConcurrentAccess verifies that SafeBuffer is safe for concurrent use.
// This test should be run with the race detector enabled: go test -race.
func TestSafeBuffer_ConcurrentAccess(t *testing.T) {
	t.Parallel()

	var sb SafeBuffer
	const numGoroutines = 100
	const writesPerGoroutine = 100
	const initialSize = 1000

	expectedLines := make([]string, 0, numGoroutines*writesPerGoroutine+initialSize)

	// Pre-populate buffer.
	for i := range initialSize {
		line := fmt.Sprintf("initial-%d", i)
		expectedLines = append(expectedLines, line)
		_, err := sb.Write([]byte(line + "\n"))
		require.NoError(t, err)
	}

	// Concurrent writers and readers.
	var wg sync.WaitGroup
	for i := range numGoroutines {
		workerLines := make([]string, 0, writesPerGoroutine)
		for j := range writesPerGoroutine {
			workerLines = append(workerLines, fmt.Sprintf("writer-%d-%d", i, j))
		}
		expectedLines = append(expectedLines, workerLines...)
		wg.Go(func() {
			for _, line := range workerLines {
				_, _ = sb.Write([]byte(line + "\n"))
			}
		})
		wg.Go(func() {
			for range writesPerGoroutine {
				_ = sb.String() // Should not race.
				_ = sb.Len()    // Should not race.
				time.Sleep(time.Microsecond)
			}
		})
	}

	wg.Wait()

	// Verify we got the writes.
	lines := strings.Split(strings.Trim(sb.String(), "\n"), "\n")
	require.ElementsMatch(t, expectedLines, lines)
}

// TestSafeBuffer_ConcurrentWriteAndReset tests concurrent writes and resets.
// This test should be run with the race detector enabled: go test -race.
func TestSafeBuffer_ConcurrentWriteAndReset(t *testing.T) {
	t.Parallel()

	var sb SafeBuffer
	const numGoroutines = 50
	const operations = 100

	// Concurrent writers and re-setters.
	var wg sync.WaitGroup
	for i := range numGoroutines {
		wg.Go(func() {
			for j := range operations {
				_, _ = sb.Write([]byte(fmt.Sprintf("data-%d-%d\n", i, j)))
			}
		})
		wg.Go(func() {
			for range operations {
				sb.Reset()
				time.Sleep(time.Microsecond)
			}
		})
	}

	wg.Wait()
	// No assertion on final state since resets are happening concurrently.
	// The test passes if no race conditions are detected.
}

// TestSafeBuffer_ConcurrentReadWrite tests concurrent reads and writes.
// This test should be run with the race detector enabled: go test -race.
func TestSafeBuffer_ConcurrentReadWrite(t *testing.T) {
	t.Parallel()

	var sb SafeBuffer
	const numGoroutines = 50
	const operations = 100

	// Pre-populate buffer
	for i := range 1000 {
		_, err := sb.Write([]byte(fmt.Sprintf("initial-%d\n", i)))
		require.NoError(t, err)
	}

	// Concurrent writers and readers.
	var wg sync.WaitGroup
	for i := range numGoroutines {
		wg.Go(func() {
			for j := range operations {
				_, _ = sb.Write([]byte(fmt.Sprintf("writer-%d-%d\n", i, j)))
			}
		})

		wg.Go(func() {
			buf := make([]byte, 10)
			for range operations {
				_, _ = sb.Read(buf) // Ignore EOF errors
				time.Sleep(time.Microsecond)
			}
		})
	}

	wg.Wait()
	// No assertion on final state since reads are happening concurrently.
	// The test passes if no race conditions are detected.
}
