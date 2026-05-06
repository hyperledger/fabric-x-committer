/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package vc

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// snapshotEntry is one key-value-version triple to write into a fixture.
type snapshotEntry struct {
	key     []byte
	value   []byte
	version uint64
}

// writeSnapshotFixture writes a genesis-data file at path with the given
// header and entries, using the same binary layout the production reader
// expects. Returns the file path.
func writeSnapshotFixture(t *testing.T, path string, hdr SnapshotHeader, entries []snapshotEntry) {
	t.Helper()
	hdrBytes, err := json.Marshal(hdr)
	require.NoError(t, err)
	require.LessOrEqual(t, len(hdrBytes), math.MaxUint32)

	f, err := os.Create(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })

	write32 := func(v uint32) {
		var buf [4]byte
		binary.BigEndian.PutUint32(buf[:], v)
		_, werr := f.Write(buf[:])
		require.NoError(t, werr)
	}
	write64 := func(v uint64) {
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], v)
		_, werr := f.Write(buf[:])
		require.NoError(t, werr)
	}
	writeBytes := func(b []byte) {
		require.LessOrEqual(t, len(b), math.MaxUint32)
		write32(uint32(len(b))) //nolint:gosec // length is bounds-checked above against math.MaxUint32.
		if len(b) > 0 {
			_, werr := f.Write(b)
			require.NoError(t, werr)
		}
	}

	writeBytes(hdrBytes)
	for _, e := range entries {
		writeBytes(e.key)
		writeBytes(e.value)
		write64(e.version)
	}
}

func defaultHeader() SnapshotHeader {
	return SnapshotHeader{
		Version:       "1",
		Namespace:     "token",
		SourceChannel: "mychannel",
		BlockHeight:   100,
		ExportedAt:    "2026-04-22T12:00:00Z",
		EntryCount:    0,
	}
}

func TestOpenSnapshotData_HeaderRoundTrip(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "genesis.bin")
	hdr := defaultHeader()
	hdr.EntryCount = 2
	writeSnapshotFixture(t, path, hdr, []snapshotEntry{
		{key: []byte("k1"), value: []byte("v1"), version: 1},
		{key: []byte("k2"), value: []byte("v2"), version: 2},
	})

	snap, err := OpenSnapshotData(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = snap.Close() })

	got := snap.Header()
	require.Equal(t, "1", got.Version)
	require.Equal(t, "token", got.Namespace)
	require.Equal(t, "mychannel", got.SourceChannel)
	require.Equal(t, uint64(100), got.BlockHeight)
	require.Equal(t, 2, got.EntryCount)
}

func TestOpenSnapshotData_EntryStream(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "genesis.bin")
	want := []snapshotEntry{
		{key: []byte("alpha"), value: []byte("aaa"), version: 10},
		{key: []byte("beta"), value: []byte("bbb"), version: 20},
		{key: []byte("gamma"), value: []byte("ccc"), version: 30},
	}
	hdr := defaultHeader()
	hdr.EntryCount = len(want)
	writeSnapshotFixture(t, path, hdr, want)

	snap, err := OpenSnapshotData(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = snap.Close() })

	var got []snapshotEntry
	for snap.Next() {
		got = append(got, snapshotEntry{
			key:     append([]byte(nil), snap.Key()...),
			value:   append([]byte(nil), snap.Value()...),
			version: snap.Version(),
		})
	}
	require.NoError(t, snap.Err())
	require.Equal(t, want, got)
}

func TestOpenSnapshotData_EmptyEntries(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "empty.bin")
	writeSnapshotFixture(t, path, defaultHeader(), nil)

	snap, err := OpenSnapshotData(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = snap.Close() })

	require.False(t, snap.Next())
	require.NoError(t, snap.Err())
}

func TestOpenSnapshotData_BinaryValues(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "binary.bin")
	want := []snapshotEntry{
		{key: []byte{0x00, 0x01, 0x02}, value: []byte{0xFF, 0xFE, 0x00}, version: 99},
	}
	hdr := defaultHeader()
	hdr.EntryCount = len(want)
	writeSnapshotFixture(t, path, hdr, want)

	snap, err := OpenSnapshotData(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = snap.Close() })

	require.True(t, snap.Next())
	require.Equal(t, want[0].key, snap.Key())
	require.Equal(t, want[0].value, snap.Value())
	require.Equal(t, want[0].version, snap.Version())
	require.False(t, snap.Next())
	require.NoError(t, snap.Err())
}

func TestOpenSnapshotData_MissingFile(t *testing.T) {
	t.Parallel()

	_, err := OpenSnapshotData(filepath.Join(t.TempDir(), "does-not-exist.bin"))
	require.Error(t, err)
}

func TestOpenSnapshotData_EmptyHeader(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "bad.bin")
	// Write a header length of zero — invalid per the format.
	f, err := os.Create(path)
	require.NoError(t, err)
	var zero [4]byte
	_, err = f.Write(zero[:])
	require.NoError(t, err)
	require.NoError(t, f.Close())

	_, err = OpenSnapshotData(path)
	require.Error(t, err)
	require.Contains(t, err.Error(), "empty header")
}

func TestOpenSnapshotData_EmptyNamespace(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "bad.bin")
	hdr := defaultHeader()
	hdr.Namespace = ""
	writeSnapshotFixture(t, path, hdr, nil)

	_, err := OpenSnapshotData(path)
	require.Error(t, err)
	require.Contains(t, err.Error(), "empty namespace")
}

func TestOpenSnapshotData_TruncatedEntry(t *testing.T) {
	t.Parallel()

	// Write a valid header followed by half of one entry's key length —
	// the iterator should surface the truncation as an error, not a clean EOF.
	path := filepath.Join(t.TempDir(), "truncated.bin")
	hdrBytes, err := json.Marshal(defaultHeader())
	require.NoError(t, err)

	f, err := os.Create(path)
	require.NoError(t, err)
	var hdrLen [4]byte
	binary.BigEndian.PutUint32(hdrLen[:], uint32(len(hdrBytes))) //nolint:gosec // header bytes < 4 GiB by construction.
	_, err = f.Write(hdrLen[:])
	require.NoError(t, err)
	_, err = f.Write(hdrBytes)
	require.NoError(t, err)
	// Two bytes of a uint32 — io.ReadFull will report ErrUnexpectedEOF.
	_, err = f.Write([]byte{0x00, 0x00})
	require.NoError(t, err)
	require.NoError(t, f.Close())

	snap, err := OpenSnapshotData(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = snap.Close() })

	require.False(t, snap.Next())
	require.Error(t, snap.Err())
}
