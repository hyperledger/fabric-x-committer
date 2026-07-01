/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package vc

import (
	"context"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/yugabyte/pgx/v5"
	"github.com/yugabyte/pgx/v5/pgxpool"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/utils/db"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring/promutil"
	"github.com/hyperledger/fabric-x-committer/utils/retry"
)

const (
	// validateReadsSQLTempl template for validating reads for each namespace.
	validateReadsSQLTempl = "SELECT * FROM validate_reads_ns_${NAMESPACE_ID}($1::BYTEA[], $2::BIGINT[]);"
	// updateNsStatesSQLTempl template for the committing updates for each namespace.
	updateNsStatesSQLTempl = "SELECT * FROM update_ns_${NAMESPACE_ID}($1::BYTEA[], $2::BYTEA[], $3::BIGINT[]);"
	// insertNsStatesSQLTempl template for committing new keys for each namespace.
	insertNsStatesSQLTempl = "SELECT * FROM insert_ns_${NAMESPACE_ID}($1::BYTEA[], $2::BYTEA[]);"
	// queryVersionsSQLTempl template for the querying versions for given keys for each namespace.
	queryVersionsSQLTempl = "SELECT key, version FROM ns_${NAMESPACE_ID} WHERE key = ANY($1);"

	// insertTxStatusSQLStmt commits transaction's status for each TX.
	insertTxStatusSQLStmt = "SELECT * FROM insert_tx_status($1::BYTEA[], $2::INTEGER[], $3::BYTEA[]);"
	// queryPoliciesSQLStmt queries meta-namespace policies.
	queryPoliciesSQLStmt = "SELECT key, value from ns_" + committerpb.MetaNamespaceID + ";"
	// queryConfigSQLStmt queries the config-namespace policy.
	queryConfigSQLStmt = "SELECT key, value from ns_" + committerpb.ConfigNamespaceID + ";"

	setMetadataPrepSQLStmt      = "UPDATE metadata SET value = $2 WHERE key = $1;"
	getMetadataPrepSQLStmt      = "SELECT value FROM metadata WHERE key = $1;"
	queryTxIDsStatusPrepSQLStmt = "SELECT tx_id, status, height FROM tx_status WHERE tx_id = ANY($1);"
)

var lastCommittedBlockNumberKey = []byte("last committed block number")

type (
	// database handles the database operations.
	database struct {
		pool                 *pgxpool.Pool
		metrics              *perfMetrics
		retryProfile         *retry.Profile
		tablePreSplitTablets int
	}

	// keyToVersion is a map from key to version.
	keyToVersion map[string]uint64

	statesToBeCommitted struct {
		updateWrites namespaceToWrites
		newWrites    namespaceToWrites
		batchStatus  *committerpb.TxStatusBatch
		txIDToHeight transactionIDToHeight
	}

	commitResult struct {
		conflicts  namespaceToReads
		duplicates []TxID
	}

	tuple[T1, T2 any] struct {
		item1 T1
		item2 T2
	}
)

// newDatabase creates a new database.
func newDatabase(ctx context.Context, config *db.Config, metrics *perfMetrics) (*database, error) {
	pool, err := db.NewPool(ctx, config)
	if err != nil {
		return nil, err
	}

	logger.Infof("validator persister connected to database at [%s]", config.EndpointsString())

	tablePreSplitTablets, err := db.GetTablePreSplitTablets(ctx, pool, config)
	if err != nil {
		pool.Close()
		return nil, err
	}

	return &database{
		pool:                 pool,
		metrics:              metrics,
		retryProfile:         config.Retry,
		tablePreSplitTablets: tablePreSplitTablets,
	}, nil
}

func (d *database) close() {
	logger.Info("closing database connection")
	d.pool.Close()
}

// validateNamespaceReads validates the reads for a given namespace.
func (d *database) validateNamespaceReads(
	ctx context.Context,
	nsID string,
	r *reads,
) (*reads /* read conflicts */, error) {
	// For each namespace nsID, we use the validate_reads_ns_<nsID> function to validate
	// the reads. This function returns the keys and versions of the conflicting reads.
	// Note that we have a table per namespace.
	// We have a validate function per namespace so that we can use the static SQL
	// to avoid parsing, planning and optimizing the query for each invoke. If we use
	// a common function for all namespace, we need to pass the table name as a parameter
	// which makes the query dynamic and hence we lose the benefits of static SQL.
	start := time.Now()
	query := db.FmtNsID(validateReadsSQLTempl, nsID)

	conflictIdx, err := retryQueryAndReadArrayResult[int](ctx, d, query, r.keys, r.versions)
	if err != nil {
		return nil, fmt.Errorf("failed to validate reads on namespace [%s]: %w", nsID, err)
	}
	readConflicts := &reads{}
	for _, i := range conflictIdx {
		// SQL indexing starts from 1.
		readConflicts.append(r.keys[i-1], r.versions[i-1])
	}
	promutil.Observe(d.metrics.databaseTxBatchValidationLatencySeconds, time.Since(start))

	return readConflicts, nil
}

// queryVersionsIfPresent queries the versions for the given keys if they exist.
func (d *database) queryVersionsIfPresent(ctx context.Context, nsID string, queryKeys [][]byte) (keyToVersion, error) {
	start := time.Now()
	query := db.FmtNsID(queryVersionsSQLTempl, nsID)

	foundKeys, foundVersions, err := retryQueryAndReadTwoItems[[]byte, int64](ctx, d, query, queryKeys)
	if err != nil {
		return nil, fmt.Errorf("failed to get keys' version from namespace [%s]: %w", nsID, err)
	}

	kToV := make(keyToVersion)
	for i, key := range foundKeys {
		kToV[string(key)] = uint64(foundVersions[i]) //nolint:gosec // DB table is constraint to non-negative value.
	}
	promutil.Observe(d.metrics.databaseTxBatchQueryVersionLatencySeconds, time.Since(start))

	return kToV, nil
}

func (d *database) getNextBlockNumberToCommit(ctx context.Context) (*servicepb.BlockRef, error) {
	value, retryErr := retry.ExecuteWithResult(ctx, d.retryProfile, func() ([]byte, error) {
		r := d.pool.QueryRow(ctx, getMetadataPrepSQLStmt, lastCommittedBlockNumberKey)
		var v []byte
		return v, errors.Wrap(r.Scan(&v), "failed to get the last committed block number")
	})
	if retryErr != nil {
		return nil, retryErr
	}
	res := &servicepb.BlockRef{
		Number: 0, // default: no block has been committed.
	}
	if len(value) > 0 {
		res.Number = binary.BigEndian.Uint64(value) + 1
	}
	return res, nil
}

func (d *database) setLastCommittedBlockNumber(ctx context.Context, bInfo *servicepb.BlockRef) error {
	// NOTE: We can actually batch this transaction with regular user transactions and perform
	//       a single commit. However, we need to implement special logic to handle cases
	//       when there are no waiting user transactions. Hence, for simplicity, we are not
	//       batching this transaction with regular user transactions. After performance
	//       benchmarking, if there is a significant benefit in performing the batching,
	//       we will implement further optimizations.

	// NOTE: As we store an integer and allow comparisons, we must ensure consistent byte representation.
	//       This means using the same length value and big-endian byte ordering for all stored integers.
	//       Both PostgreSQL and YugabyteDB support comparison of big-endian bytes through the BYTEA data type
	//       and standard comparison operators.
	v := make([]byte, 8)
	binary.BigEndian.PutUint64(v, bInfo.Number)
	return retry.ExecuteSQL(ctx, d.retryProfile, d.pool, setMetadataPrepSQLStmt, lastCommittedBlockNumberKey, v)
}

// commit commits the writes to the database.
func (d *database) commit(ctx context.Context, states *statesToBeCommitted) (*commitResult, error) {
	start := time.Now()
	if states.empty() {
		return nil, nil
	}

	// We want to commit all the writes to all namespaces or none at all,
	// so we use a database transaction. Otherwise, the failure and recovery
	// logic will be very complicated.
	tx, rollBackFunc, err := d.beginTx(ctx)
	if err != nil {
		return nil, err
	}

	// This will be executed if an error occurs. If transaction is committed, this will be a no-op.
	defer rollBackFunc()

	res, err := d.writeStatesByGroup(ctx, tx, states)
	if err != nil {
		return nil, fmt.Errorf("failed to write states: %w", err)
	}
	if res != nil {
		// rollback
		return res, nil
	}

	err = tx.Commit(ctx)
	promutil.Observe(d.metrics.databaseTxBatchCommitLatencySeconds, time.Since(start))
	if err != nil {
		return nil, errors.Wrap(err, "failed to perform the final commit on the database transaction")
	}

	return nil, nil
}

func (d *database) writeStatesByGroup(
	ctx context.Context,
	tx pgx.Tx,
	states *statesToBeCommitted,
) (*commitResult, error) {
	// Because the coordinator might submit duplicate transactions during connection issues,
	// we must insert transaction IDs first. This allows us to detect conflicts early,
	// as another vcservice instance might have already committed the same transaction.
	// If we don't insert transaction IDs first, there are other consequences. These
	// could be mitigated by adding writes with a null version present in BlindWrites
	// to the readToTxIDs map, but inserting the IDs upfront is a cleaner solution.
	duplicates, err := d.insertTxStatus(ctx, tx, states)
	if err != nil {
		return nil, fmt.Errorf("failed to insert transactions status: %w", err)
	}

	if len(duplicates) > 0 {
		// Since a duplicate ID causes a rollback, we fail fast.
		logger.Debugf("%d duplicate keys were found", len(duplicates))
		return &commitResult{duplicates: duplicates}, nil
	}

	conflicts, err := d.insertStates(ctx, tx, states.newWrites)
	if err != nil {
		return nil, fmt.Errorf("failed to insert states: %w", err)
	}

	if !conflicts.empty() {
		// Since a conflicts causes a rollback, we fail fast.
		logger.Debugf("%d read conflicts were found", len(conflicts))
		return &commitResult{conflicts: conflicts}, nil
	}

	if err = createTablesAndFunctionsForNamespaces(ctx, tx,
		states.newWrites[committerpb.MetaNamespaceID], d.tablePreSplitTablets); err != nil {
		return nil, fmt.Errorf("failed to create tables and functions for new namespaces: %w", err)
	}

	// Updates cannot have a conflicts because their versions are validated beforehand.
	if err = d.updateStates(ctx, tx, states.updateWrites); err != nil {
		return nil, fmt.Errorf("failed to execute updates: %w", err)
	}

	return nil, nil
}

func (d *database) insertTxStatus(
	ctx context.Context,
	tx pgx.Tx,
	states *statesToBeCommitted,
) ([]TxID /* duplicates */, error) {
	start := time.Now()
	if states.batchStatus == nil || len(states.batchStatus.Status) == 0 {
		return nil, nil
	}

	numEntries := len(states.batchStatus.Status)
	ids := make([][]byte, 0, numEntries)
	statues := make([]int, 0, numEntries)
	heights := make([][]byte, 0, numEntries)
	for _, status := range states.batchStatus.Status {
		// We cannot insert a "duplicate ID" status since we already have a status entry with this ID.
		if status.Status == committerpb.Status_REJECTED_DUPLICATE_TX_ID {
			continue
		}
		ids = append(ids, []byte(status.Ref.TxId))
		statues = append(statues, int(status.Status))
		blkAndTxNum, ok := states.txIDToHeight[TxID(status.Ref.TxId)]
		if !ok {
			return nil, errors.Newf("block and tx number are not passed for txID [%s]", status.Ref.TxId)
		}
		heights = append(heights, blkAndTxNum.ToBytes())
	}

	ret := tx.QueryRow(ctx, insertTxStatusSQLStmt, ids, statues, heights)
	duplicates, err := readArrayResult[[]byte](ret)
	if err != nil {
		return nil, fmt.Errorf("failed to read result from query [%s]: %w", insertTxStatusSQLStmt, err)
	}
	if len(duplicates) == 0 {
		promutil.Observe(d.metrics.databaseTxBatchCommitTxsStatusLatencySeconds, time.Since(start))
		return nil, nil
	}

	duplicateTxs := make([]TxID, len(duplicates))
	for i, v := range duplicates {
		duplicateTxs[i] = TxID(v)
	}
	logger.Debugf("Total number of duplicate txs: %d", len(duplicateTxs))
	promutil.Observe(d.metrics.databaseTxBatchCommitTxsStatusLatencySeconds, time.Since(start))
	return duplicateTxs, nil
}

func (d *database) insertStates(
	ctx context.Context, tx pgx.Tx, nsToWrites namespaceToWrites,
) (namespaceToReads /* conflicts */, error) {
	start := time.Now()
	defer func() {
		promutil.Observe(
			d.metrics.databaseTxBatchCommitInsertNewKeyWithValueLatencySeconds,
			time.Since(start),
		)
	}()

	conflicts := make(namespaceToReads)
	for nsID, writes := range nsToWrites {
		if writes.empty() {
			continue
		}

		q := db.FmtNsID(insertNsStatesSQLTempl, nsID)
		ret := tx.QueryRow(ctx, q, writes.keys, writes.values)
		violating, err := readArrayResult[[]byte](ret)
		if err != nil {
			return nil, fmt.Errorf("failed to read result from query [%s]: %w", q, err)
		}

		if len(violating) > 0 {
			// We can use arbitrary versions from the list of writes since they are all 'nil'.
			logger.Debugf("Total number of conflicts: %d for namespace [%s]", len(conflicts), nsID)
			conflicts.getOrCreate(nsID).appendMany(violating, make([]*uint64, len(violating)))
		}
	}

	if len(conflicts) > 0 {
		logger.Debugf("For all namespaces: Total number of conflicts: %v", len(conflicts))
		return conflicts, nil
	}

	return nil, nil
}

func (d *database) updateStates(ctx context.Context, tx pgx.Tx, nsToWrites namespaceToWrites) error {
	start := time.Now()
	for nsID, writes := range nsToWrites {
		if writes.empty() {
			continue
		}

		query := db.FmtNsID(updateNsStatesSQLTempl, nsID)
		_, err := tx.Exec(ctx, query, writes.keys, writes.values, writes.versions)
		if err != nil {
			return errors.Wrapf(err, "failed to execute query [%s]", query)
		}
	}
	promutil.Observe(d.metrics.databaseTxBatchCommitUpdateLatencySeconds, time.Since(start))

	return nil
}

func createTablesAndFunctionsForNamespaces(ctx context.Context, tx pgx.Tx, newNs *namespaceWrites, tablets int) error {
	if newNs == nil {
		return nil
	}

	for _, ns := range newNs.keys {
		nsID := string(ns)
		tableName := db.TableName(nsID)
		logger.Infof("Creating table [%s] and required functions for namespace [%s]", tableName, ns)
		query := db.CreateNsTables(nsID, tablets)
		if _, execErr := tx.Exec(ctx, query); execErr != nil {
			return errors.Wrapf(
				execErr,
				"failed to create table and functions for namespace [%s] with query [%s]", nsID, query,
			)
		}
	}

	return nil
}

func (d *database) beginTx(ctx context.Context) (pgx.Tx, func(), error) {
	tx, err := d.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to being a database transaction")
	}

	return tx, func() { //nolint:contextcheck // we want to rollback changes even when ctx gets cancelled.
		rollbackErr := tx.Rollback(context.Background())
		if rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
			logger.Warn("failed rolling-back transaction: ", rollbackErr)
		}
	}, nil
}

func (s *statesToBeCommitted) empty() bool {
	return s.updateWrites.empty() && s.newWrites.empty() && (s.batchStatus == nil || len(s.batchStatus.Status) == 0)
}

func (d *database) readStatusWithHeight(
	ctx context.Context,
	txIDs [][]byte,
) ([]*committerpb.TxStatus, error) {
	return retry.ExecuteWithResult(ctx, d.retryProfile, func() ([]*committerpb.TxStatus, error) {
		r, queryErr := d.pool.Query(ctx, queryTxIDsStatusPrepSQLStmt, txIDs)
		if queryErr != nil {
			return nil, errors.Wrap(queryErr, "query txIDs from the table [tx_status]")
		}
		defer r.Close()

		rows := make([]*committerpb.TxStatus, 0, len(txIDs))
		for r.Next() {
			var id []byte
			var status int32
			var height []byte

			if err := r.Scan(&id, &status, &height); err != nil {
				return nil, errors.Wrapf(err, "read rows from the query [%s] result", queryTxIDsStatusPrepSQLStmt)
			}

			ht, _, err := servicepb.NewHeightFromBytes(height)
			if err != nil {
				return nil, fmt.Errorf("create height: %w", err)
			}

			rows = append(rows, ht.WithStatus(string(id), committerpb.Status(status)))
		}
		return rows, errors.Wrap(r.Err(), "reading rows")
	})
}

func (d *database) readNamespacePolicies(ctx context.Context) (*applicationpb.NamespacePolicies, error) {
	keys, values, err := retryQueryAndReadTwoItems[[]byte, []byte](ctx, d, queryPoliciesSQLStmt)
	if err != nil {
		metaTable := db.TableName(committerpb.MetaNamespaceID)
		return nil, fmt.Errorf("failed to read the policies from table [%s]: %w", metaTable, err)
	}
	policy := &applicationpb.NamespacePolicies{
		Policies: make([]*applicationpb.PolicyItem, len(keys)),
	}

	for i, key := range keys {
		policy.Policies[i] = &applicationpb.PolicyItem{
			Namespace: string(key),
			Policy:    values[i],
		}
	}
	return policy, nil
}

func (d *database) readConfigTX(ctx context.Context) (*applicationpb.ConfigTransaction, error) {
	_, values, err := retryQueryAndReadTwoItems[[]byte, []byte](ctx, d, queryConfigSQLStmt)
	if err != nil {
		return nil, fmt.Errorf("failed to read the config transaction from table [%s]: %w",
			db.TableName(committerpb.ConfigNamespaceID), err)
	}
	configTX := &applicationpb.ConfigTransaction{}
	for _, v := range values {
		configTX.Envelope = v
	}
	return configTX, nil
}

func retryQueryAndReadArrayResult[T any](
	ctx context.Context, db *database, query string, args ...any,
) ([]T, error) {
	return retry.ExecuteWithResult(ctx, db.retryProfile, func() ([]T, error) {
		row := db.pool.QueryRow(ctx, query, args...)
		items, readErr := readArrayResult[T](row)
		if readErr != nil {
			logger.Debugf("attempt: %s", readErr)
		}
		return items, errors.Wrapf(readErr, "read rows from the query [%s] results", query)
	})
}

func retryQueryAndReadTwoItems[T1, T2 any](
	ctx context.Context, db *database, query string, args ...any,
) ([]T1, []T2, error) {
	res, err := retry.ExecuteWithResult(ctx, db.retryProfile, func() (*tuple[[]T1, []T2], error) {
		rows, queryErr := db.pool.Query(ctx, query, args...)
		if queryErr != nil {
			return nil, errors.Wrapf(queryErr, "query rows: query [%s]", query)
		}
		defer rows.Close()
		items1, items2, readErr := readTwoItems[T1, T2](rows)
		return &tuple[[]T1, []T2]{
			item1: items1,
			item2: items2,
		}, errors.Wrapf(readErr, "read rows from the query [%s] results", query)
	})
	if err != nil {
		return nil, nil, err
	}
	return res.item1, res.item2, nil
}

// readTwoItems reads two items from given rows.
func readTwoItems[T1, T2 any](r pgx.Rows) (items1 []T1, items2 []T2, err error) {
	for r.Next() {
		var i1 T1
		var i2 T2
		if err := r.Scan(&i1, &i2); err != nil {
			return nil, nil, errors.Wrap(err, "failed while scanning a row")
		}
		items1 = append(items1, i1)
		items2 = append(items2, i2)
	}
	return items1, items2, errors.Wrap(r.Err(), "failed while reading from rows")
}

func readArrayResult[T any](r pgx.Row) (res []T, err error) {
	err = r.Scan(&res)
	return res, errors.Wrap(err, "failed while scanning a row")
}
