/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package vc

import (
	"context"

	"github.com/cockroachdb/errors"
)

// BootstrapFromSnapshot initialises the validator-committer's state database
// from a genesis-data file produced by the fxmigrate exporter.
//
// The flow is one-shot:
//
//  1. Open the snapshot data file and read its header.
//  2. Connect to the configured database via a StateImporter.
//  3. Initialise the system schema (metadata, tx_status, __meta, __config).
//  4. Create the namespace declared in the header.
//  5. Stream the snapshot entries into the namespace table via PostgreSQL COPY.
//  6. Set the last-committed block number from the header so the committer
//     resumes processing at block N+1 when started normally.
//  7. Cross-check the imported row count against the header's declared count
//     to detect a truncated or partially-written snapshot file.
//  8. Verify the persisted row count and block height in the database.
//
// BootstrapFromSnapshot is intended to be run against a fresh database. If the
// target namespace already exists CreateNamespace will fail and the function
// will return that error: silently re-importing on top of existing state would
// mask operator error.
func BootstrapFromSnapshot(ctx context.Context, dbConfig *DatabaseConfig, snapshotPath string) error {
	snap, err := OpenSnapshotData(snapshotPath)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := snap.Close(); cerr != nil {
			logger.Warnf("Failed to close snapshot data file: %v", cerr)
		}
	}()

	hdr := snap.Header()
	logger.Infof(
		"Bootstrap-from-snapshot starting: namespace=%s source_channel=%s "+
			"block_height=%d declared_entries=%d",
		hdr.Namespace, hdr.SourceChannel, hdr.BlockHeight, hdr.EntryCount,
	)

	importer, err := NewStateImporterFromConfig(ctx, dbConfig)
	if err != nil {
		return err
	}
	defer importer.Close()

	if err = importer.InitSchema(ctx); err != nil {
		return errors.Wrap(err, "failed to init schema")
	}
	if err = importer.CreateNamespace(ctx, hdr.Namespace); err != nil {
		return errors.Wrapf(err, "failed to create namespace [%s]", hdr.Namespace)
	}

	rowCount, err := importer.ImportNamespaceState(ctx, hdr.Namespace, snap)
	if err != nil {
		return err
	}
	logger.Infof("Imported %d rows into namespace [%s]", rowCount, hdr.Namespace)

	if int64(hdr.EntryCount) != rowCount {
		return errors.Newf(
			"snapshot integrity violation: header declares %d entries but %d were imported",
			hdr.EntryCount, rowCount,
		)
	}

	if err = importer.SetBlockHeight(ctx, hdr.BlockHeight); err != nil {
		return err
	}

	summary := &ImportSummary{
		NamespaceRowCounts: map[string]int64{hdr.Namespace: rowCount},
		BlockNumber:        hdr.BlockHeight,
		TotalKeys:          rowCount,
	}
	if err = importer.VerifyImport(ctx, summary); err != nil {
		return errors.Wrap(err, "failed to verify import")
	}

	logger.Infof(
		"Bootstrap-from-snapshot complete: namespace=%s imported_rows=%d block_height=%d",
		hdr.Namespace, rowCount, hdr.BlockHeight,
	)
	return nil
}
