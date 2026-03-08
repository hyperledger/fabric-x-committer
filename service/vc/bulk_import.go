/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package vc

import (
	"context"
	"fmt"

	"github.com/cockroachdb/errors"
	"github.com/yugabyte/pgx/v5"
)

// BulkImportData represents the data to be bulk-imported for a single namespace.
// Keys, Values, and Versions are parallel slices that must be the same length.
type BulkImportData struct {
	NsID     string
	Keys     [][]byte
	Values   [][]byte
	Versions []uint64
}

// BulkImportResult stores per-namespace row counts after a successful bulk import.
type BulkImportResult struct {
	NamespaceCounts map[string]uint64
}

// ErrDatabaseNotEmpty indicates that a bulk import was attempted on a database
// that already has committed blocks. Bulk import is only permitted on an empty,
// freshly initialized database.
var ErrDatabaseNotEmpty = errors.New("bulk import rejected: database already contains committed blocks")

// bulkImport populates the state database with the given key-value data using PostgreSQL's
// binary COPY protocol for maximum throughput. This method is intended to be used during
// the bootstrap-from-snapshot workflow, where a Fabric peer snapshot is ingested into
// the Fabric-X committer's state database.
//
// Preconditions:
//   - The system tables must have been created via setupSystemTablesAndNamespaces.
//   - The database must be empty (no committed blocks yet).
//
// The entire import is performed within a single database transaction, ensuring
// atomicity: either all namespace data is imported or none is.
func (db *database) bulkImport(ctx context.Context, data []*BulkImportData) (*BulkImportResult, error) {
	if len(data) == 0 {
		return &BulkImportResult{NamespaceCounts: map[string]uint64{}}, nil
	}

	if err := validateBulkImportData(data); err != nil {
		return nil, err
	}

	if err := db.ensureDatabaseEmpty(ctx); err != nil {
		return nil, err
	}

	tx, rollBackFunc, err := db.beginTx(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to begin bulk import transaction: %w", err)
	}
	defer rollBackFunc()

	result := &BulkImportResult{
		NamespaceCounts: make(map[string]uint64, len(data)),
	}

	createdTables := make(map[string]bool)
	for _, d := range data {
		if err := db.ensureNamespaceTable(ctx, tx, d.NsID, createdTables); err != nil {
			return nil, err
		}

		copied, err := db.copyNamespaceData(ctx, tx, d)
		if err != nil {
			return nil, err
		}

		result.NamespaceCounts[d.NsID] += uint64(copied) //nolint:gosec // copied is always non-negative.
	}

	if err := db.verifyImportCounts(ctx, tx, result.NamespaceCounts); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, errors.Wrap(err, "failed to commit bulk import transaction")
	}

	logger.Infof("Bulk import completed successfully: %d namespaces imported", len(result.NamespaceCounts))
	for nsID, count := range result.NamespaceCounts {
		logger.Infof("  namespace [%s]: %d keys", nsID, count)
	}

	return result, nil
}

// validateBulkImportData checks that all import data entries are well-formed.
func validateBulkImportData(data []*BulkImportData) error {
	for _, d := range data {
		if d.NsID == "" {
			return errors.New("bulk import data contains an entry with an empty namespace ID")
		}
		if len(d.Keys) != len(d.Values) || len(d.Keys) != len(d.Versions) {
			return errors.Newf(
				"bulk import data for namespace [%s] has mismatched slice lengths: keys=%d, values=%d, versions=%d",
				d.NsID, len(d.Keys), len(d.Values), len(d.Versions),
			)
		}
	}
	return nil
}

// ensureDatabaseEmpty verifies that no blocks have been committed yet.
func (db *database) ensureDatabaseEmpty(ctx context.Context) error {
	nextBlock, err := db.getNextBlockNumberToCommit(ctx)
	if err != nil {
		return fmt.Errorf("failed to check database state: %w", err)
	}
	if nextBlock.Number > 0 {
		return ErrDatabaseNotEmpty
	}
	return nil
}

// ensureNamespaceTable creates the namespace table and its SQL functions if they
// haven't been created yet during this import session.
func (db *database) ensureNamespaceTable(
	ctx context.Context, tx pgx.Tx, nsID string, created map[string]bool,
) error {
	if created[nsID] {
		return nil
	}

	err := createNsTables(nsID, db.tablePreSplitTablets, func(q string) error {
		_, execErr := tx.Exec(ctx, q)
		return execErr
	})
	if err != nil {
		return fmt.Errorf("failed to create table for namespace [%s]: %w", nsID, err)
	}

	created[nsID] = true
	logger.Infof("Created table for namespace [%s]", nsID)
	return nil
}

// copyNamespaceData uses PostgreSQL's COPY protocol to bulk-load rows into a namespace table.
func (db *database) copyNamespaceData(ctx context.Context, tx pgx.Tx, d *BulkImportData) (int64, error) {
	if len(d.Keys) == 0 {
		return 0, nil
	}

	tableName := pgx.Identifier{TableName(d.NsID)}
	columns := []string{"key", "value", "version"}

	copied, err := tx.CopyFrom(ctx, tableName, columns,
		pgx.CopyFromSlice(len(d.Keys), func(i int) ([]any, error) {
			return []any{d.Keys[i], d.Values[i], int64(d.Versions[i])}, nil //nolint:gosec // version constraint enforced by DB.
		}),
	)
	if err != nil {
		return 0, errors.Wrapf(err, "failed to COPY data into namespace [%s]", d.NsID)
	}

	logger.Debugf("Copied %d rows into namespace [%s]", copied, d.NsID)
	return copied, nil
}

// verifyImportCounts checks that the actual row counts in each namespace table
// match the expected counts from the COPY operations.
func (db *database) verifyImportCounts(
	ctx context.Context, tx pgx.Tx, expected map[string]uint64,
) error {
	for nsID, expectedCount := range expected {
		var actualCount uint64
		query := fmt.Sprintf("SELECT count(*) FROM %s", TableName(nsID)) //nolint:gosec // nsID is not user-supplied.
		if err := tx.QueryRow(ctx, query).Scan(&actualCount); err != nil {
			return errors.Wrapf(err, "failed to verify row count for namespace [%s]", nsID)
		}
		if actualCount != expectedCount {
			return errors.Newf(
				"count mismatch for namespace [%s]: expected %d rows, found %d",
				nsID, expectedCount, actualCount,
			)
		}
	}
	return nil
}
