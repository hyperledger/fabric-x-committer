/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package query

import (
	"context"
	"crypto/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/google/uuid"
	"github.com/hyperledger/fabric-lib-go/common/flogging"
	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/hyperledger/fabric-x-common/protoutil"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/hyperledger/fabric-x-committer/service/vc"
	"github.com/hyperledger/fabric-x-committer/service/verifier/policy"
	"github.com/hyperledger/fabric-x-committer/utils"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/dynamictls"
	"github.com/hyperledger/fabric-x-committer/utils/grpcerror"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring/promutil"
)

var logger = flogging.MustGetLogger("query-service")

var (
	// ErrInvalidOrStaleView is returned when attempting to use wrong, stale, or cancelled view.
	ErrInvalidOrStaleView = errors.New("invalid or stale view")

	// ErrTooManyKeys is returned when the number of keys in a request exceeds the configured limit.
	ErrTooManyKeys = errors.New("request exceeds maximum allowed keys")

	// ErrEmptyNamespaces is returned when a query request does not contain any namespaces.
	ErrEmptyNamespaces = errors.New("query namespaces must not be empty")

	// ErrEmptyKeys is returned when a namespace query does not contain any keys.
	ErrEmptyKeys = errors.New("query namespace keys must not be empty")

	// ErrEmptyTxIDs is returned when a transaction status query has no transaction IDs.
	ErrEmptyTxIDs = errors.New("transaction status query tx_ids must not be empty")

	// ErrTooManyActiveViews is returned when the number of active views exceeds the configured limit.
	ErrTooManyActiveViews = errors.New("active view limit exceeded")
)

type (
	// Service is a gRPC service that implements the QueryServiceServer interface.
	Service struct {
		committerpb.UnimplementedQueryServiceServer
		batcher     viewsBatcher
		config      *Config
		metrics     *perfMetrics
		ready       *channel.Ready
		healthcheck *health.Server

		// Dynamic CA management fields: dynamicRootCAs provides atomic, restart-free certificate
		// rotation; lastCAFetch tracks the last refresh to a throttle database load; and
		// refreshMutex implements a double-check locking pattern to prevent redundant,
		// concurrent refresh operations during connection spikes.
		dynamicRootCAs atomic.Pointer[[][]byte]
		lastCAFetch    atomic.Int64
		refreshMutex   sync.Mutex
	}
)

// caFetchTimeout is the maximum time allowed for a single CA certificate fetch from the database tryout.
// On failure, the existing CAs are kept and the next incoming connection will immediately retry.
// This timeout prevents a slow or unresponsive database from blocking TLS handshakes indefinitely.
const caFetchTimeout = 15 * time.Second

// NewQueryService create a new QueryService given a configuration.
func NewQueryService(config *Config) *Service {
	return &Service{
		config:         config,
		metrics:        newQueryServiceMetrics(),
		ready:          channel.NewReady(),
		healthcheck:    connection.DefaultHealthCheckService(),
		dynamicRootCAs: atomic.Pointer[[][]byte]{},
	}
}

// WaitForReady waits for the service resources to initialize, so it is ready to answers requests.
// If the context ended before the service is ready, returns false.
func (q *Service) WaitForReady(ctx context.Context) bool {
	return q.ready.WaitForReady(ctx)
}

// Run starts the Prometheus server.
func (q *Service) Run(ctx context.Context) error {
	pool, poolErr := vc.NewDatabasePool(ctx, q.config.Database)
	if poolErr != nil {
		return poolErr
	}
	defer pool.Close()

	q.batcher = viewsBatcher{
		ctx:         ctx,
		config:      q.config,
		metrics:     q.metrics,
		pool:        pool,
		viewLimiter: utils.NewConcurrencyLimiter(q.config.MaxActiveViews),
		nonConsistentBatcher: batcher{
			ctx: ctx,
			cancel: func() {
			},
			config:   q.config,
			metrics:  q.metrics,
			queryObj: &sharedPool{pool: pool},
		},
	}
	q.ready.SignalReady()

	_ = q.metrics.StartPrometheusServer(ctx, q.config.Monitoring)
	// We don't use the error here as we avoid stopping the service due to monitoring error.
	<-ctx.Done()
	return nil
}

// RegisterService registers for the query-service's GRPC services.
func (q *Service) RegisterService(server *grpc.Server) {
	committerpb.RegisterQueryServiceServer(server, q)
	healthgrpc.RegisterHealthServer(server, q.healthcheck)
}

// GetDynamicRootCAs returns the current CA certificates for the query service's gRPC server.
// It caches the CAs and only refreshes them periodically to avoid excessive database queries.
func (q *Service) GetDynamicRootCAs() *atomic.Pointer[[][]byte] {
	now := time.Now().Unix()
	lastFetch := q.lastCAFetch.Load()

	if lastFetch == 0 || (now-lastFetch) > q.config.CAFetchInterval.Nanoseconds() {
		q.refreshDynamicRootCAs()
	}

	return &q.dynamicRootCAs
}

// refreshDynamicRootCAs uses a double-check locking pattern to ensure only one
// goroutine performs the expensive config fetch.
// Concurrent callers arriving during a refresh will block on the mutex and skip the fetch once released.
func (q *Service) refreshDynamicRootCAs() {
	// Acquire mutex to ensure only one goroutine performs the refresh at a time.
	// This prevents a problem where multiple of concurrent client
	// connections could each trigger a separate database query for the same config block.
	q.refreshMutex.Lock()
	defer q.refreshMutex.Unlock()

	// Double-check pattern: After acquiring the lock, verify that another goroutine
	// hasn't already completed the refresh while waiting for the lock.
	now := time.Now().Unix()
	lastFetch := q.lastCAFetch.Load()
	if lastFetch != 0 && (now-lastFetch) <= q.config.CAFetchInterval.Nanoseconds() {
		// Another goroutine just refreshed the CAs while waiting for the lock.
		// The data is fresh, so we can skip the database query.
		return
	}

	logger.Debug("Refreshing dynamic root CAs from config transaction")

	// TODO: Add version optimization, We don't need to read the config-block entirely, only the version.
	ctx, cancel := context.WithTimeout(context.Background(), caFetchTimeout)
	defer cancel()

	configTx, err := q.GetConfigTransaction(ctx, nil)
	if err != nil {
		logger.Warnf("Failed to fetch config transaction for dynamic CAs: %v", err)
		return // Keep existing CAs
	}

	envelope, err := protoutil.UnmarshalEnvelope(configTx.Envelope)
	if err != nil {
		logger.Warnf("Failed to unmarshal config envelope: %v", err)
		return // Keep existing CAs
	}

	rootCAs, err := dynamictls.NewOrganizationsFromEnvelope(envelope)
	if err != nil {
		logger.Warnf("Failed to extract root CAs from config: %v", err)
		return // Keep existing CAs
	}

	if len(rootCAs) == 0 {
		logger.Warn("no CA certificates found in config block")
	}
	q.dynamicRootCAs.Store(&rootCAs)
	q.lastCAFetch.Store(time.Now().UnixNano())
	logger.Debugf("Refreshed %d root CAs from config transaction", len(rootCAs))
}

// BeginView implements the query-service interface.
func (q *Service) BeginView(
	ctx context.Context, params *committerpb.ViewParameters,
) (*committerpb.View, error) {
	q.metrics.requests.WithLabelValues(grpcBeginView).Inc()
	defer q.requestLatency(grpcBeginView, time.Now())

	// Validate and cap timeout.
	if params.TimeoutMilliseconds == 0 ||
		int64(params.TimeoutMilliseconds) > q.config.MaxViewTimeout.Milliseconds() { //nolint:gosec
		params.TimeoutMilliseconds = uint64(q.config.MaxViewTimeout.Milliseconds()) //nolint:gosec
	}

	// Generate unique view ID and create view.
	// We try again if we have view-id collision.
	for ctx.Err() == nil {
		viewID, err := getUUID()
		if err != nil {
			return nil, grpcerror.WrapInternalError(err)
		}
		err = q.batcher.makeView(ctx, viewID, params)
		if err == nil {
			return &committerpb.View{Id: viewID}, nil
		}
		if errors.Is(err, errViewIDCollision) {
			continue
		}
		if errors.Is(err, ErrTooManyActiveViews) {
			return nil, grpcerror.WrapResourceExhaustedOrCancelled(ctx,
				errors.Wrapf(err, "limit %d", q.config.MaxActiveViews),
			)
		}
		return nil, grpcerror.WrapInternalError(err)
	}
	return nil, grpcerror.WrapCancelled(ctx.Err())
}

// EndView implements the query-service interface.
func (q *Service) EndView(
	_ context.Context, view *committerpb.View,
) (*emptypb.Empty, error) {
	q.metrics.requests.WithLabelValues(grpcEndView).Inc()
	defer q.requestLatency(grpcEndView, time.Now())
	return nil, grpcerror.WrapFailedPrecondition(q.batcher.removeViewID(view.Id))
}

// GetRows implements the query-service interface.
func (q *Service) GetRows(
	ctx context.Context, query *committerpb.Query,
) (*committerpb.Rows, error) {
	q.metrics.requests.WithLabelValues(grpcGetRows).Inc()

	if len(query.Namespaces) == 0 {
		return nil, grpcerror.WrapInvalidArgument(ErrEmptyNamespaces)
	}

	for _, ns := range query.Namespaces {
		err := policy.ValidateNamespaceID(ns.NsId)
		if err != nil {
			return nil, grpcerror.WrapInvalidArgument(err)
		}
		if len(ns.Keys) == 0 {
			return nil, grpcerror.WrapInvalidArgument(errors.Wrapf(ErrEmptyKeys, "namespace %s", ns.NsId))
		}
	}

	totalKeys := 0
	for _, ns := range query.Namespaces {
		totalKeys += len(ns.Keys)
	}
	if err := q.validateKeysCount(totalKeys); err != nil {
		return nil, err
	}

	defer q.requestLatency(grpcGetRows, time.Now())
	promutil.AddToCounter(q.metrics.keysRequested, totalKeys)

	batches, err := q.assignRequest(ctx, query)
	if err != nil {
		return nil, wrapQueryError(err)
	}

	res := &committerpb.Rows{
		Namespaces: make([]*committerpb.RowsNamespace, len(query.Namespaces)),
	}
	for i, ns := range query.Namespaces {
		resRows, _, resErr := batches[i].waitForRows(ctx, ns.Keys)
		if resErr != nil {
			return nil, wrapQueryError(resErr)
		}
		res.Namespaces[i] = &committerpb.RowsNamespace{
			NsId: ns.NsId,
			Rows: resRows,
		}
		promutil.AddToCounter(q.metrics.keysResponded, len(resRows))
	}
	return res, nil
}

// GetTransactionStatus implements the query-service interface.
func (q *Service) GetTransactionStatus(
	ctx context.Context, query *committerpb.TxStatusQuery,
) (*committerpb.TxStatusResponse, error) {
	q.metrics.requests.WithLabelValues(grpcGetTxStatus).Inc()

	if len(query.TxIds) == 0 {
		return nil, grpcerror.WrapInvalidArgument(ErrEmptyTxIDs)
	}

	if err := q.validateKeysCount(len(query.TxIds)); err != nil {
		return nil, err
	}

	defer q.requestLatency(grpcGetTxStatus, time.Now())

	keys := make([][]byte, len(query.TxIds))
	for i, txID := range query.TxIds {
		keys[i] = []byte(txID)
	}

	batches, err := q.assignRequest(ctx, &committerpb.Query{
		View: query.View,
		Namespaces: []*committerpb.QueryNamespace{{
			NsId: txStatusNsID,
			Keys: keys,
		}},
	})
	if err != nil {
		return nil, wrapQueryError(err)
	}

	res := &committerpb.TxStatusResponse{}
	_, resRows, resErr := batches[0].waitForRows(ctx, keys)
	if resErr != nil {
		return nil, wrapQueryError(resErr)
	}
	res.Statuses = resRows
	promutil.AddToCounter(q.metrics.keysResponded, len(resRows))
	return res, nil
}

// GetNamespacePolicies implements the query-service interface.
func (q *Service) GetNamespacePolicies(
	ctx context.Context,
	_ *emptypb.Empty,
) (*applicationpb.NamespacePolicies, error) {
	res, err := queryPolicies(ctx, q.batcher.pool)
	return res, grpcerror.WrapInternalError(err)
}

// GetConfigTransaction implements the query-service interface.
func (q *Service) GetConfigTransaction(
	ctx context.Context,
	_ *emptypb.Empty,
) (*applicationpb.ConfigTransaction, error) {
	res, err := queryConfig(ctx, q.batcher.pool)
	return res, grpcerror.WrapInternalError(err)
}

func (q *Service) assignRequest(
	ctx context.Context, query *committerpb.Query,
) ([]*namespaceQueryBatch, error) {
	defer func(start time.Time) {
		promutil.Observe(q.metrics.requestAssignmentLatencySeconds, time.Since(start))
	}(time.Now())
	batcher, err := q.batcher.getBatcher(ctx, query.View)
	if err != nil {
		return nil, err
	}

	batches := make([]*namespaceQueryBatch, len(query.Namespaces))
	for i, ns := range query.Namespaces {
		batches[i], err = batcher.addNamespaceKeys(ctx, ns.NsId, ns.Keys)
		if err != nil {
			return nil, err
		}
	}
	return batches, nil
}

func getUUID() (string, error) {
	uuidObj, err := uuid.NewRandomFromReader(rand.Reader)
	if err != nil {
		return "", err
	}
	return uuidObj.String(), nil
}

func (q *Service) validateKeysCount(count int) error {
	if q.config.MaxRequestKeys > 0 && count > q.config.MaxRequestKeys {
		return grpcerror.WrapInvalidArgument(
			errors.Join(ErrTooManyKeys, errors.Newf("requested %d keys, maximum allowed is %d",
				count, q.config.MaxRequestKeys)))
	}
	return nil
}

func (q *Service) requestLatency(method string, start time.Time) {
	promutil.Observe(q.metrics.requestsLatency.WithLabelValues(method), time.Since(start))
}

// wrapQueryError wraps query errors with appropriate gRPC status codes.
func wrapQueryError(err error) error {
	if err == nil {
		return nil
	}

	if errors.Is(err, ErrInvalidOrStaleView) {
		return grpcerror.WrapFailedPrecondition(err)
	}

	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return grpcerror.WrapCancelled(err)
	}

	return grpcerror.WrapInternalError(err)
}
