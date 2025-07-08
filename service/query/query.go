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

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
	"github.com/hyperledger/fabric-x-committer/api/protoqueryservice"
	"github.com/hyperledger/fabric-x-committer/api/types"
	"github.com/hyperledger/fabric-x-committer/service/vc"
)

//go:embed query.sql
var queryRowSQLTemplate string

var (
	queryPoliciesStmt = fmt.Sprintf("SELECT key, value, version from ns_%s;", types.MetaNamespaceID)
	queryConfigStmt   = fmt.Sprintf("SELECT key, value, version from ns_%s;", types.ConfigNamespaceID)
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
) ([]*protoqueryservice.Row, error) {
	queryStmt := vc.FmtNsID(queryRowSQLTemplate, nsID)
	r, err := queryObj.Query(ctx, queryStmt, keys)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return readQueryRows(r, len(keys))
}

func queryPolicies(ctx context.Context, queryObj querier) (*protoblocktx.NamespacePolicies, error) {
	r, err := queryObj.Query(ctx, queryPoliciesStmt)
	if err != nil {
		return nil, errors.Wrap(err, "failed to query policies")
	}
	defer r.Close()
	rows, err := readQueryRows(r, 1)
	if err != nil {
		return nil, err
	}
	policy := &protoblocktx.NamespacePolicies{
		Policies: make([]*protoblocktx.PolicyItem, len(rows)),
	}
	for i, row := range rows {
		policy.Policies[i] = &protoblocktx.PolicyItem{
			Namespace: string(row.Key),
			Policy:    row.Value,
			Version:   row.Version,
		}
	}
	return policy, nil
}

func queryConfig(ctx context.Context, queryObj querier) (*protoblocktx.ConfigTransaction, error) {
	r, err := queryObj.Query(ctx, queryConfigStmt)
	if err != nil {
		return nil, errors.Wrap(err, "failed to query policies")
	}
	defer r.Close()
	rows, err := readQueryRows(r, 1)
	if err != nil {
		return nil, err
	}
	configTX := &protoblocktx.ConfigTransaction{}
	for _, row := range rows {
		configTX.Envelope = row.Value
		configTX.Version = row.Version
	}
	return configTX, nil
}

func readQueryRows(r pgx.Rows, expectedSize int) ([]*protoqueryservice.Row, error) {
	rows := make([]*protoqueryservice.Row, 0, expectedSize)
	for r.Next() {
		v := &protoqueryservice.Row{}
		if err := r.Scan(&v.Key, &v.Value, &v.Version); err != nil {
			return nil, err
		}
		rows = append(rows, v)
	}
	return rows, r.Err()
}
