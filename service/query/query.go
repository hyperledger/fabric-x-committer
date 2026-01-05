/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package query

import (
	"context"
	_ "embed"
	"fmt"
	"sync"

	"github.com/cockroachdb/errors"
	"github.com/yugabyte/pgx/v4"
	"github.com/yugabyte/pgx/v4/pgxpool"

	"github.com/hyperledger/fabric-x-committer/api/applicationpb"
	"github.com/hyperledger/fabric-x-committer/api/committerpb"
	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/service/vc"
)

//go:embed query_tmpl.sql
var queryRowSQLTemplate string

var (
	queryPoliciesStmt    = fmt.Sprintf("SELECT key, value, version from ns_%s;", committerpb.MetaNamespaceID)
	queryConfigStmt      = fmt.Sprintf("SELECT key, value, version from ns_%s;", committerpb.ConfigNamespaceID)
	queryTxIDsStatusStmt = "SELECT tx_id, status, height FROM tx_status WHERE tx_id = ANY($1);"
)

// sharedPool and sharedLazyTx implements of sharedQuerier.
// sharedPool returns a new connection (implements acquiredQuerier)
// from the pool for every acquire.
// sharedLazyTx locks the transaction to make sure only one goroutine
// uses the connection, and returns itself (implements acquiredQuerier).
// This is because a transaction's connection cannot be used concurrently.
// It also creates the transaction lazily if it was not created yet.
type (
	sharedPool struct {
		pool *pgxpool.Pool
	}
	sharedLazyTx struct {
		pgx.Tx
		m sync.Mutex
		//nolint:containedctx // used to restrict jobs to the batcher context.
		ctx     context.Context
		metrics *perfMetrics
		options *pgx.TxOptions
		pool    *pgxpool.Pool
	}
	sharedQuerier interface {
		Acquire(ctx context.Context) (acquiredQuerier, error)
	}
	acquiredQuerier interface {
		querier
		Release()
	}
	querier interface {
		Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	}
)

func (t *sharedPool) Acquire(ctx context.Context) (acquiredQuerier, error) {
	return t.pool.Acquire(ctx)
}

func (t *sharedLazyTx) Acquire(ctx context.Context) (acquiredQuerier, error) {
	t.m.Lock()
	m := t.metrics.processingSessions.WithLabelValues(sessionTransactions)
	if t.Tx == nil {
		dbTx, err := t.pool.BeginTx(ctx, *t.options)
		if err != nil {
			t.m.Unlock()
			return nil, err
		}
		t.Tx = dbTx
		m.Inc()
		context.AfterFunc(t.ctx, func() { //nolint:contextcheck // false positive.
			m.Dec()
			// We use background context to rollback transactions to avoid
			// dangling transactions in the database.
			_ = dbTx.Rollback(context.Background())
		})
	}
	return t, nil
}

func (t *sharedLazyTx) Release() {
	t.m.Unlock()
}

// unsafeQueryRows queries rows for the given keys.
// It may not support concurrent execution, depending on the querier implementation.
func unsafeQueryRows(
	ctx context.Context, queryObj querier, nsID string, keys [][]byte,
) ([]*committerpb.Row, error) {
	queryStmt := vc.FmtNsID(queryRowSQLTemplate, nsID)
	r, err := queryObj.Query(ctx, queryStmt, keys)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return readQueryRows(r, len(keys))
}

// unsafeQueryTxStatus queries rows from the tx_status table.
// It may not support concurrent execution, depending on the querier implementation.
func unsafeQueryTxStatus(
	ctx context.Context, queryObj querier, txIDs [][]byte,
) ([]*committerpb.TxStatus, error) {
	r, err := queryObj.Query(ctx, queryTxIDsStatusStmt, txIDs)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return readTxStatusRows(r, len(txIDs))
}

func queryPolicies(ctx context.Context, queryObj querier) (*applicationpb.NamespacePolicies, error) {
	r, err := queryObj.Query(ctx, queryPoliciesStmt)
	if err != nil {
		return nil, errors.Wrap(err, "failed to query policies")
	}
	defer r.Close()
	rows, err := readQueryRows(r, 1)
	if err != nil {
		return nil, err
	}
	policy := &applicationpb.NamespacePolicies{
		Policies: make([]*applicationpb.PolicyItem, len(rows)),
	}

	for i, row := range rows {
		policy.Policies[i] = &applicationpb.PolicyItem{
			Namespace: string(row.Key),
			Policy:    row.Value,
			Version:   row.Version,
		}
	}
	return policy, nil
}

func queryConfig(ctx context.Context, queryObj querier) (*applicationpb.ConfigTransaction, error) {
	r, err := queryObj.Query(ctx, queryConfigStmt)
	if err != nil {
		return nil, errors.Wrap(err, "failed to query policies")
	}
	defer r.Close()
	rows, err := readQueryRows(r, 1)
	if err != nil {
		return nil, err
	}
	configTX := &applicationpb.ConfigTransaction{}
	for _, row := range rows {
		configTX.Envelope = row.Value
		configTX.Version = row.Version
	}
	return configTX, nil
}

func readQueryRows(r pgx.Rows, expectedSize int) ([]*committerpb.Row, error) {
	rows := make([]*committerpb.Row, 0, expectedSize)
	for r.Next() {
		v := &committerpb.Row{}
		if err := r.Scan(&v.Key, &v.Value, &v.Version); err != nil {
			return nil, err
		}
		rows = append(rows, v)
	}
	return rows, r.Err()
}

func readTxStatusRows(r pgx.Rows, expectedSize int) ([]*committerpb.TxStatus, error) {
	rows := make([]*committerpb.TxStatus, 0, expectedSize)
	for r.Next() {
		var id []byte
		var status int32
		var height []byte

		if err := r.Scan(&id, &status, &height); err != nil {
			return nil, errors.Wrap(err, "failed to read rows from the query result")
		}

		ht, _, err := servicepb.NewHeightFromBytes(height)
		if err != nil {
			return nil, errors.Wrap(err, "failed to create height")
		}

		rows = append(rows, &committerpb.TxStatus{
			Ref:    committerpb.NewTxRef(string(id), ht.BlockNum, ht.TxNum),
			Status: committerpb.Status(status),
		})
	}
	return rows, r.Err()
}
