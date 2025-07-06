/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package query

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/yugabyte/pgx/v4"
	"github.com/yugabyte/pgx/v4/pgxpool"

	"github.com/hyperledger/fabric-x-committer/api/protoqueryservice"
	"github.com/hyperledger/fabric-x-committer/utils"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring/promutil"
)

// viewParametersPermutations there is a constant number of view-parameter permutations.
// Four options for the isolation level, and two options for deferred/non-deferred.
const (
	minIsoLevel = 0 // 0b000
	maxIsoLevel = 3 // 0b011
	deferredBit = 4 // 0b100
)

// viewsBatcher is designed to allow maximal concurrency between requests.
// It uses lock-free structures to ensure minimal contention.
// batcher aggregates queries.
// It is referred by all the viewHolder(s) that has the same
// parameters and are within the same aggregation window.
// We also have an additional unique batcher for the purpose of queries
// that do not use consistent view (nonConsistentBatcher).
// namespaceQueryBatch holds all the keys within the same namespace that
// are to be queried in this batch.
// It is referred by a batcher until the batch is finalized,
// then it can be replaced by a new one.
// It is also referred by all the grpc callers that inserted keys into it.
// This allows them to fetch the results once the query is done.
type (
	viewsBatcher struct {
		//nolint:containedctx // used to restrict jobs to the runtime context.
		ctx     context.Context
		config  *Config
		metrics *perfMetrics
		pool    *pgxpool.Pool

		// viewIDToViewHolder maps view-id to *viewHolder.
		// viewParametersToLatestBatcher maps view-parameters-key to *batcher.
		// nonConsistentBatcher holds the batcher for any non-consistent queries.
		viewIDToViewHolder            utils.SyncMap[string, *viewHolder]
		viewParametersToLatestBatcher utils.SyncMap[int, *batcher]
		nonConsistentBatcher          batcher
	}
	viewHolder struct {
		m sync.Mutex
		//nolint:containedctx // used to restrict jobs to the view timeout.
		ctx    context.Context
		cancel context.CancelFunc
		params *protoqueryservice.ViewParameters

		// batcher is initialized lazily upon the first query.
		batcher *batcher
	}
	batcher struct {
		m sync.Mutex
		//nolint:containedctx // used to restrict jobs to the view and runtime timeout.
		ctx        context.Context
		cancel     context.CancelFunc
		config     *Config
		metrics    *perfMetrics
		queryObj   sharedQuerier
		refCounter uint64

		// nsToLatestQueryBatch maps a namespace ID to its latest *namespaceQueryBatch.
		nsToLatestQueryBatch utils.SyncMap[string, *namespaceQueryBatch]
	}
	namespaceQueryBatch struct {
		m sync.Mutex
		//nolint:containedctx // used to restrict waiting queries to query life span.
		ctx      context.Context
		cancel   context.CancelCauseFunc
		metrics  *perfMetrics
		queryObj sharedQuerier

		nsID    string
		keys    [][]byte
		minSize int
		created time.Time

		// sync.Once is used to ensure execute will only be called once.
		// submit might be called more than once because minSize and the
		// timeout can occur simultaneously, but only one will call execute.
		submitter sync.Once

		// finalized is true once the query is actively executed.
		finalized bool
		result    map[string]*protoqueryservice.Row
	}
)

// makeView attempts to create a view with the given view ID.
// It returns true if the view ID was not used.
func (q *viewsBatcher) makeView(
	viewID string, p *protoqueryservice.ViewParameters,
) bool {
	ctx, cancel := context.WithTimeout(q.ctx, time.Duration(p.TimeoutMilliseconds)*time.Millisecond) //nolint:gosec
	v := &viewHolder{
		ctx:    ctx,
		cancel: cancel,
		params: p,
	}
	if _, loaded := q.viewIDToViewHolder.LoadOrStore(viewID, v); loaded {
		cancel()
		return false
	}

	m := q.metrics.processingSessions.WithLabelValues(sessionViews)
	m.Inc()
	context.AfterFunc(ctx, func() {
		m.Dec()
		q.viewIDToViewHolder.CompareAndDelete(viewID, v)

		v.m.Lock()
		b := v.batcher
		v.batcher = nil
		v.m.Unlock()
		if b != nil {
			b.leave()
		}
	})
	return true
}

// getBatcher returns the view's assigned batcher, and assigns one if it was not assigned.
func (q *viewsBatcher) getBatcher(ctx context.Context, view *protoqueryservice.View) (*batcher, error) {
	if view == nil {
		return &q.nonConsistentBatcher, nil
	}
	v, ok := q.viewIDToViewHolder.Load(view.Id)
	if !ok {
		return nil, ErrInvalidOrStaleView
	}

	v.m.Lock()
	defer v.m.Unlock()
	if v.batcher == nil {
		if _, err := q.updateOrCreateBatcher(ctx, v); err != nil {
			return nil, err
		}
	}
	if v.batcher.isStale() {
		return nil, ErrInvalidOrStaleView
	}
	return v.batcher, nil
}

// updateOrCreateBatcher assigns a new or existing batcher to a viewHolder.
// It assumes the viewHolder is locked.
func (q *viewsBatcher) updateOrCreateBatcher(
	ctx context.Context, v *viewHolder,
) (*batcher, error) {
	key := viewParametersKey(v.params)
	return mapUpdateOrCreate(
		ctx,
		&q.viewParametersToLatestBatcher,
		key,
		updateOrCreate[batcher]{
			create: func() *batcher {
				// If the returned batcher fails to be assigned,
				// this object will be disregarded and the following
				// context will never be canceled.
				// Thus, we make sure to not use the context until
				// its assignment.
				// We use the context later in the post method, which
				// is called only after a successful assignment.
				batcherCtx, batcherCancel := context.WithCancel(q.ctx)
				return &batcher{
					ctx:    batcherCtx,
					cancel: batcherCancel,
					queryObj: &sharedLazyTx{
						ctx:     batcherCtx,
						options: makeTxOptions(v.params),
						metrics: q.metrics,
						pool:    q.pool,
					},
					config:  q.config,
					metrics: q.metrics,
				}
			},
			post: func(batcher *batcher) {
				// After the aggregation window, we remove the batcher from the listing.
				t := time.AfterFunc(q.config.ViewAggregationWindow, func() {
					q.viewParametersToLatestBatcher.CompareAndDelete(key, batcher)
				})
				context.AfterFunc(batcher.ctx, func() {
					t.Stop()
					q.viewParametersToLatestBatcher.CompareAndDelete(key, batcher)
				})
			},
			update: func(batcher *batcher) bool {
				return batcher.join(v)
			},
		},
	)
}

// viewParametersKey outputs a unique key for each parameter's permutation.
func viewParametersKey(p *protoqueryservice.ViewParameters) int {
	index := max(minIsoLevel, min(int(p.IsoLevel), maxIsoLevel))
	if p.NonDeferrable {
		index |= deferredBit
	}
	return index
}

// makeTxOptions translates view parameters to TxOptions.
func makeTxOptions(p *protoqueryservice.ViewParameters) *pgx.TxOptions {
	o := &pgx.TxOptions{
		AccessMode:     pgx.ReadOnly,
		IsoLevel:       pgx.Serializable,
		DeferrableMode: pgx.Deferrable,
	}
	if p == nil {
		return o
	}
	switch p.IsoLevel {
	case protoqueryservice.IsoLevel_Serializable:
		o.IsoLevel = pgx.Serializable
	case protoqueryservice.IsoLevel_RepeatableRead:
		o.IsoLevel = pgx.RepeatableRead
	case protoqueryservice.IsoLevel_ReadCommitted:
		o.IsoLevel = pgx.ReadCommitted
	case protoqueryservice.IsoLevel_ReadUncommitted:
		o.IsoLevel = pgx.ReadUncommitted
	}
	if p.NonDeferrable {
		o.DeferrableMode = pgx.NotDeferrable
	} else {
		o.DeferrableMode = pgx.Deferrable
	}
	return o
}

// removeViewID releases a view.
func (q *viewsBatcher) removeViewID(viewID string) error {
	v, ok := q.viewIDToViewHolder.LoadAndDelete(viewID)
	if !ok {
		return ErrInvalidOrStaleView
	}
	v.cancel()
	return nil
}

func (b *batcher) isStale() bool {
	return b.ctx.Err() != nil
}

// join assigns a view to the batcher.
// It returns true if it was successfully assigned.
// It assumes the viewHolder is locked.
func (b *batcher) join(v *viewHolder) bool {
	b.m.Lock()
	defer b.m.Unlock()
	if b.isStale() || b.refCounter >= uint64(b.config.MaxAggregatedViews) { //nolint:gosec
		return false
	}
	b.refCounter++
	v.batcher = b
	context.AfterFunc(b.ctx, v.cancel)
	return true
}

// leave releases a view's assignment to a batcher.
// Once no more viewIDToViewHolder are assigned, the batcher is released.
func (b *batcher) leave() {
	b.m.Lock()
	defer b.m.Unlock()
	if b.refCounter > 0 {
		b.refCounter--
	}
	if b.refCounter == 0 {
		b.cancel()
	}
}

// addNamespaceKeys add the given keys to the latest query batch.
// It creates a new query batch if needed.
func (b *batcher) addNamespaceKeys(ctx context.Context, nsID string, keys [][]byte) (*namespaceQueryBatch, error) {
	return mapUpdateOrCreate(
		ctx,
		&b.nsToLatestQueryBatch,
		nsID,
		updateOrCreate[namespaceQueryBatch]{
			create: func() *namespaceQueryBatch {
				// If the returned namespaceQueryBatch fails to be assigned,
				// this object will be disregarded and the following
				// context will never be canceled.
				// Thus, we make sure to not use the context until
				// its assignment.
				// We use the context later in the post method, which
				// is called only after a successful assignment.
				queryCtx, queryCancel := context.WithCancelCause(b.ctx)
				return &namespaceQueryBatch{
					ctx:      queryCtx,
					cancel:   queryCancel,
					metrics:  b.metrics,
					queryObj: b.queryObj,
					nsID:     nsID,
					minSize:  b.config.MinBatchKeys,
					created:  time.Now(),
				}
			},
			post: func(q *namespaceQueryBatch) {
				m := b.metrics.processingSessions.WithLabelValues(sessionProcessingQueries)
				m.Inc()
				t := time.AfterFunc(b.config.MaxBatchWait, q.submit)
				context.AfterFunc(q.ctx, func() {
					t.Stop()
					m.Dec()
				})
			},
			update: func(q *namespaceQueryBatch) bool {
				return q.addKeys(keys)
			},
		},
	)
}

// addKeys returns true if the batch isn't finalized.
func (q *namespaceQueryBatch) addKeys(
	keys [][]byte,
) bool {
	if q.ctx.Err() != nil {
		return false
	}

	q.m.Lock()
	defer q.m.Unlock()
	if q.finalized {
		return false
	}
	if q.keys == nil {
		// We preallocate the keys slice to prevent repeated reallocations under the lock.
		q.keys = make([][]byte, 0, q.minSize*2)
	}
	q.keys = append(q.keys, keys...)
	if len(q.keys) >= q.minSize {
		q.submit()
	}
	return true
}

// submit fires the execute method only once in a go routine.
func (q *namespaceQueryBatch) submit() {
	q.submitter.Do(func() {
		go q.execute()
	})
}

// execute attempts to execute the query.
// It does not support parallel execution and should only be called once using the submit method.
func (q *namespaceQueryBatch) execute() {
	if q.ctx.Err() != nil {
		return
	}

	m := q.metrics.processingSessions.WithLabelValues(sessionWaitingQueries)
	m.Inc()
	// We make sure we have an available connection before we finalize the batch.
	queryObj, err := q.queryObj.Acquire(q.ctx)
	m.Dec()
	if err != nil {
		q.cancel(err)
		return
	}
	defer queryObj.Release()
	q.m.Lock()
	q.finalized = true
	q.m.Unlock()

	m = q.metrics.processingSessions.WithLabelValues(sessionInExecutionQueries)
	m.Inc()
	defer m.Dec()

	q.result = make(map[string]*protoqueryservice.Row)
	var uniqueKeys [][]byte
	for _, key := range q.keys {
		strKey := string(key)
		if _, ok := q.result[strKey]; !ok {
			q.result[strKey] = nil
			uniqueKeys = append(uniqueKeys, key)
		}
	}

	promutil.Observe(q.metrics.batchQueuingTimeSeconds, time.Since(q.created))
	promutil.ObserveSize(q.metrics.batchQuerySize, len(uniqueKeys))

	start := time.Now()
	rows, err := unsafeQueryRows(q.ctx, queryObj, q.nsID, uniqueKeys)
	if err != nil {
		q.cancel(err)
		return
	}
	promutil.Observe(q.metrics.queryLatencySeconds, time.Since(start))
	promutil.ObserveSize(q.metrics.batchResponseSize, len(rows))

	for _, r := range rows {
		q.result[string(r.Key)] = r
	}
	q.cancel(nil)
}

// waitForRows waits for the batch query to end and fetches the provided subset of keys.
// If the query failed or the provided context ended, it returns the cause.
func (q *namespaceQueryBatch) waitForRows(
	ctx context.Context,
	keys [][]byte,
) ([]*protoqueryservice.Row, error) {
	// Wait for batch to be finalized or context to be canceled.
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-q.ctx.Done():
		// Query completed.
	}

	if errors.Is(q.ctx.Err(), context.DeadlineExceeded) {
		return nil, ErrInvalidOrStaleView
	}
	// context.Cause() returns context.Canceled when err=nil is reported.
	if err := context.Cause(q.ctx); !errors.Is(err, context.Canceled) { //nolint:contextcheck // false positive.
		return nil, err
	}

	// Extract results for requested keys.
	res := make([]*protoqueryservice.Row, 0, len(keys))
	for _, key := range keys {
		// Get result for this key from the batch results.
		if row, ok := q.result[string(key)]; ok && row != nil {
			res = append(res, row)
		}
	}
	return res, nil
}
