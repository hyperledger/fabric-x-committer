/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package query

import (
	"context"
	"crypto/rand"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/google/uuid"
	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/hyperledger/fabric-x-committer/service/vc"
	"github.com/hyperledger/fabric-x-committer/service/verifier/policy"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/grpcerror"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring/promutil"
)

var (
	// ErrInvalidOrStaleView is returned when attempting to use wrong, stale, or cancelled view.
	ErrInvalidOrStaleView = errors.New("invalid or stale view")

	// ErrTooManyKeys is returned when the number of keys in a request exceeds the configured limit.
	ErrTooManyKeys = errors.New("request exceeds maximum allowed keys")
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
	}
)

// NewQueryService create a new QueryService given a configuration.
func NewQueryService(config *Config) *Service {
	return &Service{
		config:      config,
		metrics:     newQueryServiceMetrics(),
		ready:       channel.NewReady(),
		healthcheck: connection.DefaultHealthCheckService(),
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
		ctx:     ctx,
		config:  q.config,
		metrics: q.metrics,
		pool:    pool,
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

	_ = q.metrics.StartPrometheusServer(ctx, q.config.Monitoring.Server)
	// We don't use the error here as we avoid stopping the service due to monitoring error.
	<-ctx.Done()
	return nil
}

// RegisterService registers for the query-service's GRPC services.
func (q *Service) RegisterService(server *grpc.Server) {
	committerpb.RegisterQueryServiceServer(server, q)
	healthgrpc.RegisterHealthServer(server, q.healthcheck)
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
		if q.batcher.makeView(viewID, params) { //nolint:contextcheck // false positive.
			return &committerpb.View{Id: viewID}, nil
		}
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

	for _, ns := range query.Namespaces {
		err := policy.ValidateNamespaceID(ns.NsId)
		if err != nil {
			return nil, grpcerror.WrapInvalidArgument(err)
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
