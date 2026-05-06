/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package vc

import (
	"context"
	"encoding/binary"
	"fmt"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/yugabyte/pgx/v5"
)

// StateImporter provides bulk state import capabilities for initializing the
// committer's state database from an external source such as a Fabric peer snapshot.
//
// Unlike the normal transaction processing pipeline (preparer -> validator -> committer),
// imported state bypasses MVCC validation and endorsement verification since it originates
// from a trusted, pre-validated source. This makes it suitable for the bootstrap-from-snapshot
// startup mode where the committer initializes its state database from a migration data file.
//
// Usage:
//
//	importer := NewStateImporter(db)
//	_ = importer.InitSchema(ctx)
//	_ = importer.CreateNamespace(ctx, "myns")
//	count, _ := importer.ImportNamespaceState(ctx, "myns", iterator)
//	_ = importer.SetBlockHeight(ctx, snapshotBlockNum)
//	_ = importer.VerifyNamespaceRowCount(ctx, "myns", expectedCount)
type StateImporter struct {
	db *database
}

// NewStateImporter creates a StateImporter backed by the given database.
// This is intended for use within the vc package and tests.
func NewStateImporter(db *database) *StateImporter {
	return &StateImporter{db: db}
}

// NewStateImporterFromConfig creates a StateImporter by connecting to the database
// described by the given configuration. The caller is responsible for calling Close()
// when the importer is no longer needed.
func NewStateImporterFromConfig(ctx context.Context, config *DatabaseConfig) (*StateImporter, error) {
	metrics := newVCServiceMetrics()
	db, err := newDatabase(ctx, config, metrics)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create database for state import")
	}
	return &StateImporter{db: db}, nil
}

// Close releases the database connection pool held by the importer.
func (si *StateImporter) Close() {
	si.db.close()
}

// StateIterator provides a streaming interface for reading key-value-version
// triples during bulk state import. Implementations should return data in
// whatever order the source provides; the importer does not require sorted input.
//
// Callers must check Err() after Next() returns false to distinguish EOF from failure.
type StateIterator interface {
	// Next advances the iterator to the next record. Returns false when exhausted or on error.
	Next() bool
	// Key returns the current record's key. Only valid after a successful Next() call.
	Key() []byte
	// Value returns the current record's value. Only valid after a successful Next() call.
	Value() []byte
	// Version returns the current record's MVCC version. Only valid after a successful Next() call.
	Version() uint64
	// Err returns any error encountered during iteration.
	Err() error
}

// ImportSummary contains statistics collected during a bulk import operation.
// It can be used with VerifyImport to validate integrity after import completes.
type ImportSummary struct {
	// NamespaceRowCounts maps each namespace ID to the number of rows imported.
	NamespaceRowCounts map[string]int64
	// BlockNumber is the snapshot block height that was set during import.
	BlockNumber uint64
	// TotalKeys is the sum of all rows across all namespaces.
	TotalKeys int64
}

// InitSchema creates the system tables (metadata, tx_status) and system namespace
// tables (__meta, __config). This must be called before any other import operation.
func (si *StateImporter) InitSchema(ctx context.Context) error {
	return si.db.setupSystemTablesAndNamespaces(ctx)
}

// CreateNamespace creates the table and stored functions for a user-defined namespace.
// The namespace must not already exist. System namespaces (__meta, __config) are created
// by InitSchema and should not be passed here.
func (si *StateImporter) CreateNamespace(ctx context.Context, nsID string) error {
	return createNsTables(nsID, si.db.tablePreSplitTablets, func(q string) error {
		_, err := si.db.pool.Exec(ctx, q)
		return err
	})
}

// ImportNamespaceState bulk-inserts key-value-version state into a namespace table
// using PostgreSQL's COPY protocol for high throughput.
//
// The namespace table must already exist (created via CreateNamespace or InitSchema).
// Returns the number of rows imported. The iterator is consumed fully; callers should
// check iter.Err() after this method returns for any source-side errors.
func (si *StateImporter) ImportNamespaceState(
	ctx context.Context,
	nsID string,
	iter StateIterator,
) (int64, error) {
	tableName := pgx.Identifier{TableName(nsID)}
	columns := []string{"key", "value", "version"}

	src := &copyFromIterator{iter: iter}
	count, err := si.db.pool.CopyFrom(ctx, tableName, columns, src)
	if err != nil {
		return 0, errors.Wrapf(err, "COPY into namespace [%s] failed", nsID)
	}

	if iterErr := iter.Err(); iterErr != nil {
		return count, errors.Wrapf(iterErr, "iterator error after importing namespace [%s]", nsID)
	}

	logger.Infof("Imported %d rows into namespace [%s]", count, nsID)
	return count, nil
}

// ImportPolicy inserts a namespace policy entry into the __meta system namespace.
// This is used during snapshot import to register the endorsement policies for
// each migrated namespace so that the coordinator can recover them on startup.
func (si *StateImporter) ImportPolicy(ctx context.Context, nsID string, policy []byte) error {
	q := FmtNsID(insertNsStatesSQLTempl, committerpb.MetaNamespaceID)
	ret := si.db.pool.QueryRow(ctx, q, [][]byte{[]byte(nsID)}, [][]byte{policy})
	violating, err := readArrayResult[[]byte](ret)
	if err != nil {
		return errors.Wrapf(err, "failed to import policy for namespace [%s]", nsID)
	}
	if len(violating) > 0 {
		return errors.Newf("policy for namespace [%s] already exists", nsID)
	}
	return nil
}

// SetBlockHeight sets the last committed block number in the metadata table.
// After import, the committer will resume processing from blockNumber+1.
func (si *StateImporter) SetBlockHeight(ctx context.Context, blockNumber uint64) error {
	v := make([]byte, 8)
	binary.BigEndian.PutUint64(v, blockNumber)
	_, err := si.db.pool.Exec(ctx, setMetadataPrepSQLStmt, lastCommittedBlockNumberKey, v)
	return errors.Wrap(err, "failed to set block height after import")
}

// VerifyNamespaceRowCount checks that the actual number of rows in a namespace table
// matches the expected count. Returns nil if they match, or an error describing the mismatch.
func (si *StateImporter) VerifyNamespaceRowCount(ctx context.Context, nsID string, expected int64) error {
	var actual int64
	query := fmt.Sprintf("SELECT count(*) FROM %s", TableName(nsID))
	if err := si.db.pool.QueryRow(ctx, query).Scan(&actual); err != nil {
		return errors.Wrapf(err, "failed to count rows in namespace [%s]", nsID)
	}
	if actual != expected {
		return errors.Newf(
			"row count mismatch for namespace [%s]: expected %d, got %d",
			nsID, expected, actual,
		)
	}
	return nil
}

// VerifyImport validates the integrity of a completed import by checking row counts
// for every namespace in the summary and verifying the stored block height.
func (si *StateImporter) VerifyImport(ctx context.Context, expected *ImportSummary) error {
	for nsID, expectedCount := range expected.NamespaceRowCounts {
		if err := si.VerifyNamespaceRowCount(ctx, nsID, expectedCount); err != nil {
			return err
		}
	}

	blkRef, err := si.db.getNextBlockNumberToCommit(ctx)
	if err != nil {
		return errors.Wrap(err, "failed to read block height during verification")
	}

	// getNextBlockNumberToCommit returns lastCommitted+1, so we compare against expected+1.
	if blkRef.Number != expected.BlockNumber+1 {
		return errors.Newf(
			"block height mismatch: expected next block %d, got %d",
			expected.BlockNumber+1, blkRef.Number,
		)
	}

	return nil
}

// copyFromIterator adapts a StateIterator to pgx.CopyFromSource.
type copyFromIterator struct {
	iter StateIterator
}

func (c *copyFromIterator) Next() bool {
	return c.iter.Next()
}

func (c *copyFromIterator) Values() ([]any, error) {
	return []any{c.iter.Key(), c.iter.Value(), int64(c.iter.Version())}, nil //nolint:gosec
}

func (c *copyFromIterator) Err() error {
	return c.iter.Err()
}
