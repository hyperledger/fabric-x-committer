/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package vc

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"io"
	"os"

	"github.com/cockroachdb/errors"
)

// snapshotReadBufferSize is the size of the buffered reader wrapping the
// snapshot file. 1 MiB is the same buffer size the producer (fxmigrate) uses,
// keeping read patterns symmetric.
const snapshotReadBufferSize = 1 << 20

// SnapshotHeader is the JSON header at the start of a genesis-data file
// produced by the fxmigrate exporter. The on-wire format is specified in
// fabric-x-rfcs PR #5.
type SnapshotHeader struct {
	Version       string `json:"version"`
	Namespace     string `json:"namespace"`
	SourceChannel string `json:"source_channel"`
	BlockHeight   uint64 `json:"block_height"`
	ExportedAt    string `json:"exported_at"`
	EntryCount    int    `json:"entry_count"`
}

// SnapshotData is a streaming reader over a genesis-data file. After opening,
// the parsed header is available via Header(); the entries that follow are
// read one at a time through the StateIterator methods (Next, Key, Value,
// Version, Err). Close must be called when iteration is finished.
//
// SnapshotData satisfies the StateIterator interface so it can be passed
// directly to StateImporter.ImportNamespaceState without an adapter.
//
// File format (big-endian):
//
//	[header_len uint32][header JSON bytes]
//	repeated:
//	    [key_len uint32][key bytes]
//	    [value_len uint32][value bytes]
//	    [version uint64]
type SnapshotData struct {
	file   *os.File
	reader *bufio.Reader
	header *SnapshotHeader

	// current record, valid only after a successful Next().
	key     []byte
	value   []byte
	version uint64

	// sticky error: once set by a failing Next, all subsequent Next calls
	// short-circuit to false and Err() reports the failure.
	err error
}

// OpenSnapshotData opens the genesis-data file at path and reads its header.
// The returned SnapshotData is positioned at the first entry and ready for
// Next. The caller is responsible for calling Close.
func OpenSnapshotData(path string) (*SnapshotData, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.Wrap(err, "failed to open snapshot data file")
	}

	s := &SnapshotData{
		file:   f,
		reader: bufio.NewReaderSize(f, snapshotReadBufferSize),
	}

	hdr, err := s.readHeader()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	s.header = hdr
	return s, nil
}

// Header returns the parsed snapshot header. Available immediately after
// OpenSnapshotData returns.
func (s *SnapshotData) Header() *SnapshotHeader {
	return s.header
}

// Close releases the underlying file handle.
func (s *SnapshotData) Close() error {
	if err := s.file.Close(); err != nil {
		return errors.Wrap(err, "closing snapshot data file")
	}
	return nil
}

// Next advances to the next entry. Returns false at end-of-file or on error.
// Use Err to distinguish a clean EOF (nil) from a read failure.
func (s *SnapshotData) Next() bool {
	if s.err != nil {
		return false
	}

	key, err := s.readLenPrefixed()
	if errors.Is(err, io.EOF) {
		return false
	}
	if err != nil {
		s.err = errors.Wrap(err, "reading entry key")
		return false
	}

	value, err := s.readLenPrefixed()
	if err != nil {
		s.err = errors.Wrap(err, "reading entry value")
		return false
	}

	version, err := s.readUint64()
	if err != nil {
		s.err = errors.Wrap(err, "reading entry version")
		return false
	}

	s.key = key
	s.value = value
	s.version = version
	return true
}

// Key returns the current record's key. Only valid after a successful Next.
func (s *SnapshotData) Key() []byte { return s.key }

// Value returns the current record's value. Only valid after a successful Next.
func (s *SnapshotData) Value() []byte { return s.value }

// Version returns the current record's MVCC version. Only valid after a
// successful Next.
func (s *SnapshotData) Version() uint64 { return s.version }

// Err returns any error encountered during iteration. nil indicates a clean
// end-of-file.
func (s *SnapshotData) Err() error { return s.err }

func (s *SnapshotData) readHeader() (*SnapshotHeader, error) {
	hdrLen, err := s.readUint32()
	if err != nil {
		return nil, errors.Wrap(err, "reading header length")
	}
	if hdrLen == 0 {
		return nil, errors.New("snapshot data file has empty header")
	}

	hdrBytes := make([]byte, hdrLen)
	if _, err := io.ReadFull(s.reader, hdrBytes); err != nil {
		return nil, errors.Wrap(err, "reading header bytes")
	}

	var hdr SnapshotHeader
	if err := json.Unmarshal(hdrBytes, &hdr); err != nil {
		return nil, errors.Wrap(err, "parsing header JSON")
	}

	if hdr.Namespace == "" {
		return nil, errors.New("snapshot header has empty namespace")
	}
	return &hdr, nil
}

func (s *SnapshotData) readLenPrefixed() ([]byte, error) {
	n, err := s.readUint32()
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, nil
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(s.reader, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func (s *SnapshotData) readUint32() (uint32, error) {
	var buf [4]byte
	if _, err := io.ReadFull(s.reader, buf[:]); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(buf[:]), nil
}

func (s *SnapshotData) readUint64() (uint64, error) {
	var buf [8]byte
	if _, err := io.ReadFull(s.reader, buf[:]); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(buf[:]), nil
}
