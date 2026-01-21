/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package query

import (
	"context"
	"encoding/base64"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"github.com/yugabyte/pgx/v4/pgxpool"
	"google.golang.org/grpc/status"

	"github.com/hyperledger/fabric-x-committer/loadgen"
	"github.com/hyperledger/fabric-x-committer/loadgen/adapters"
	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/service/vc"
	"github.com/hyperledger/fabric-x-committer/service/verifier/policy"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
	"github.com/hyperledger/fabric-x-committer/utils/signature"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

type (
	queryServiceTestEnv struct {
		config        *Config
		qs            *Service
		ns            []string
		clientConn    committerpb.QueryServiceClient
		pool          *pgxpool.Pool
		disabledViews []string
	}

	queryServiceTestOpts struct {
		serverTLS      connection.TLSConfig
		clientTLS      connection.TLSConfig
		maxRequestKeys int
	}
)

// TestQuerySecureConnection verifies the query service gRPC server's behavior
// under various client TLS configurations.
func TestQuerySecureConnection(t *testing.T) {
	t.Parallel()
	test.RunSecureConnectionTest(t,
		func(t *testing.T, serverTLS, clientTLS connection.TLSConfig) test.RPCAttempt {
			t.Helper()
			env := newQueryServiceTestEnv(t, &queryServiceTestOpts{serverTLS: serverTLS, clientTLS: clientTLS})
			return func(ctx context.Context, t *testing.T, cfg connection.TLSConfig) error {
				t.Helper()
				client := createQueryClientWithTLS(t, &env.qs.config.Server.Endpoint, cfg)
				_, err := client.GetConfigTransaction(ctx, nil)
				return err
			}
		},
	)
}

func TestQuery(t *testing.T) {
	t.Parallel()
	env := newQueryServiceTestEnv(t, nil)
	requiredItems := env.makeItems(t)
	query, _, _ := makeQuery(requiredItems)

	for i, qNs := range query.Namespaces {
		expectedItem := requiredItems[i]
		qNs := qNs
		t.Run(fmt.Sprintf("Query internal NS %s", qNs.NsId), func(t *testing.T) {
			t.Parallel()
			ret, err := unsafeQueryRows(t.Context(), env.pool, qNs.NsId, qNs.Keys)
			require.NoError(t, err)
			requireRow(t, expectedItem, &committerpb.RowsNamespace{
				NsId: qNs.NsId,
				Rows: ret,
			})
		})
	}

	t.Run("Query GetRows interface", func(t *testing.T) {
		t.Parallel()
		ret, err := env.qs.GetRows(t.Context(), query)
		require.NoError(t, err)
		requireResults(t, requiredItems, ret.Namespaces)
	})

	t.Run("Query GetRows client", func(t *testing.T) {
		t.Parallel()
		ret, err := env.clientConn.GetRows(t.Context(), query)
		require.NoError(t, err)
		requireResults(t, requiredItems, ret.Namespaces)
	})

	t.Run("Query GetRows bad namespace ID", func(t *testing.T) {
		t.Parallel()
		badQuery, _, _ := makeQuery(requiredItems)
		badQuery.Namespaces[0].NsId = "$1"
		ret, err := env.clientConn.GetRows(t.Context(), badQuery)
		require.Error(t, err)
		require.Nil(t, ret)
		require.Contains(t, err.Error(), policy.ErrInvalidNamespaceID.Error())
	})

	t.Run("Query GetRows client with view", func(t *testing.T) {
		t.Parallel()
		ret, err := env.clientConn.GetRows(t.Context(), &committerpb.Query{
			View:       env.beginView(t, env.clientConn, defaultViewParams(time.Minute)),
			Namespaces: query.Namespaces,
		})
		require.NoError(t, err)
		requireResults(t, requiredItems, ret.Namespaces)
	})

	t.Run("Nil view parameters", func(t *testing.T) {
		t.Parallel()
		env.beginView(t, env.clientConn, nil)
	})

	t.Run("Bad view ID", func(t *testing.T) {
		t.Parallel()
		_, err := env.clientConn.EndView(t.Context(), &committerpb.View{Id: "bad"})
		require.Equal(t, ErrInvalidOrStaleView.Error(), status.Convert(err).Message())
	})

	t.Run("Cancelled view ID", func(t *testing.T) {
		t.Parallel()
		view, err := env.clientConn.BeginView(t.Context(), defaultViewParams(time.Minute))
		require.NoError(t, err)
		_, err = env.clientConn.EndView(t.Context(), view)
		require.NoError(t, err)
		_, err = env.clientConn.EndView(t.Context(), view)
		require.Equal(t, ErrInvalidOrStaleView.Error(), status.Convert(err).Message())
	})

	t.Run("Expired view ID", func(t *testing.T) {
		t.Parallel()
		view, err := env.clientConn.BeginView(t.Context(), defaultViewParams(100*time.Millisecond))
		require.NoError(t, err)
		time.Sleep(150 * time.Millisecond)
		_, err = env.clientConn.EndView(t.Context(), view)
		require.ErrorContains(t, err, status.Convert(err).Message())
	})
}

func TestMaxRequestKeys(t *testing.T) {
	t.Parallel()

	t.Run("GetRows exceeds limit", func(t *testing.T) {
		t.Parallel()
		env := newQueryServiceTestEnv(t, &queryServiceTestOpts{maxRequestKeys: 5})
		env.makeItems(t)

		// Request with 6 keys across namespaces should fail (limit is 5)
		query := &committerpb.Query{
			Namespaces: []*committerpb.QueryNamespace{
				{NsId: "0", Keys: strToBytes("item1", "item2", "item3")},
				{NsId: "1", Keys: strToBytes("item1", "item2", "item3")},
			},
		}
		_, err := env.clientConn.GetRows(t.Context(), query)
		require.Error(t, err)
		require.ErrorContains(t, err, ErrTooManyKeys.Error())
		require.ErrorContains(t, err, "requested 6 keys")
	})

	t.Run("GetRows within limit", func(t *testing.T) {
		t.Parallel()
		env := newQueryServiceTestEnv(t, &queryServiceTestOpts{maxRequestKeys: 10})
		env.makeItems(t)

		// Request with 6 keys should succeed (limit is 10)
		query := &committerpb.Query{
			Namespaces: []*committerpb.QueryNamespace{
				{NsId: "0", Keys: strToBytes("item1", "item2", "item3")},
				{NsId: "1", Keys: strToBytes("item1", "item2", "item3")},
			},
		}
		ret, err := env.clientConn.GetRows(t.Context(), query)
		require.NoError(t, err)
		require.Len(t, ret.Namespaces, 2)
	})

	t.Run("GetRows no limit when zero", func(t *testing.T) {
		t.Parallel()
		env := newQueryServiceTestEnv(t, nil)
		env.makeItems(t)

		// Request with many keys should succeed when limit is 0 (disabled)
		query := &committerpb.Query{
			Namespaces: []*committerpb.QueryNamespace{
				{NsId: "0", Keys: strToBytes("item1", "item2", "item3", "item4")},
				{NsId: "1", Keys: strToBytes("item1", "item2", "item3", "item4")},
				{NsId: "2", Keys: strToBytes("item1", "item2", "item3", "item4")},
			},
		}
		ret, err := env.clientConn.GetRows(t.Context(), query)
		require.NoError(t, err)
		require.Len(t, ret.Namespaces, 3)
	})

	t.Run("GetTransactionStatus exceeds limit", func(t *testing.T) {
		t.Parallel()
		env := newQueryServiceTestEnv(t, &queryServiceTestOpts{maxRequestKeys: 2})

		// Request with 3 transaction IDs should fail (limit is 2)
		_, err := env.clientConn.GetTransactionStatus(t.Context(), &committerpb.TxStatusQuery{
			TxIds: []string{"tx1", "tx2", "tx3"},
		})
		require.Error(t, err)
		require.ErrorContains(t, err, ErrTooManyKeys.Error())
		require.ErrorContains(t, err, "requested 3 keys")
	})

	t.Run("GetTransactionStatus within limit", func(t *testing.T) {
		t.Parallel()
		env := newQueryServiceTestEnv(t, &queryServiceTestOpts{maxRequestKeys: 5})

		// Request with 2 transaction IDs should succeed (limit is 5)
		ret, err := env.clientConn.GetTransactionStatus(t.Context(), &committerpb.TxStatusQuery{
			TxIds: []string{"tx1", "tx2"},
		})
		require.NoError(t, err)
		// The transactions don't exist, but the request should be accepted
		require.Empty(t, ret.Statuses)
	})
}

func TestQueryMetrics(t *testing.T) {
	t.Parallel()
	env := newQueryServiceTestEnv(t, nil)
	requiredItems := env.makeItems(t)
	query, keyCount, querySize := makeQuery(requiredItems)

	t.Log("Query GetRows client with view")
	view0 := env.beginView(t, env.clientConn, defaultViewParams(time.Minute))
	ret, err := env.clientConn.GetRows(t.Context(), &committerpb.Query{
		View:       view0,
		Namespaces: query.Namespaces,
	})
	require.NoError(t, err)
	requireResults(t, requiredItems, ret.Namespaces)

	t.Log("Validate metrics")
	require.Equal(t, 1, env.qs.batcher.viewIDToViewHolder.Count())
	requireIntVecMetricValue(t, 1, env.qs.metrics.requests.MetricVec, grpcBeginView)
	test.RequireIntMetricValue(t, 1, env.qs.metrics.processingSessions.WithLabelValues(sessionViews))
	test.RequireIntMetricValue(t, 1, env.qs.metrics.processingSessions.WithLabelValues(sessionTransactions))
	env.endView(t, env.clientConn, view0)
	require.Equal(t, 0, env.qs.batcher.viewIDToViewHolder.Count())

	for range 3 {
		ret, err = env.qs.GetRows(t.Context(), query)
		require.NoError(t, err)
		requireResults(t, requiredItems, ret.Namespaces)
	}

	expectedMetricsSize := 4
	require.Equal(t, 0, env.qs.batcher.viewIDToViewHolder.Count())
	test.RequireIntMetricValue(t, 0, env.qs.metrics.processingSessions.WithLabelValues(sessionViews))
	test.RequireIntMetricValue(t, 0, env.qs.metrics.processingSessions.WithLabelValues(sessionTransactions))
	requireIntVecMetricValue(t, expectedMetricsSize, env.qs.metrics.requests.MetricVec, grpcGetRows)
	test.RequireIntMetricValue(t, expectedMetricsSize*querySize, env.qs.metrics.keysRequested)
	test.RequireIntMetricValue(t, expectedMetricsSize*keyCount, env.qs.metrics.keysResponded)
}

func TestQueryWithConsistentView(t *testing.T) {
	t.Parallel()
	env := newQueryServiceTestEnv(t, nil)
	requiredItems := env.makeItems(t)
	query, _, _ := makeQuery(requiredItems)

	client := env.clientConn

	t.Log("Query GetRows client with view")
	view0 := env.beginView(t, client, defaultViewParams(time.Minute))
	ret, err := client.GetRows(t.Context(), &committerpb.Query{
		View:       view0,
		Namespaces: query.Namespaces,
	})
	require.NoError(t, err)
	requireResults(t, requiredItems, ret.Namespaces)

	// This part will query key1, then update key2.
	// We expect that the updated key2 will not be visible to GetRows() with the initial view.
	// But it will be visible to a new view.
	t.Log("Consistency with repeated GetRows")
	t1 := requiredItems[0]
	testItem1 := items{"0", t1.keys[:1], t1.values[:1], t1.versions[:1]}

	view1 := env.beginView(t, client, defaultViewParams(time.Minute))
	ret, err = client.GetRows(t.Context(), &committerpb.Query{
		View: view1,
		Namespaces: []*committerpb.QueryNamespace{{
			NsId: testItem1.ns,
			Keys: testItem1.keys,
		}},
	})
	require.NoError(t, err)

	requireResults(t, []*items{&testItem1}, ret.Namespaces)

	testItem2 := items{testItem1.ns, t1.keys[1:2], t1.values[1:2], t1.versions[1:2]}
	testItem2Mod := testItem2
	testItem2Mod.values = strToBytes("value2/1")
	testItem2Mod.versions = []uint64{2}
	env.update(t, &testItem2Mod)

	key2Query := &committerpb.Query{
		View: view1,
		Namespaces: []*committerpb.QueryNamespace{{
			NsId: testItem2.ns,
			Keys: testItem2.keys,
		}},
	}
	ret2, err := client.GetRows(t.Context(), key2Query)
	require.NoError(t, err)
	// This is the same view, so we expect the old version of item 2.
	requireResults(t, []*items{&testItem2}, ret2.Namespaces)

	view2 := env.beginView(t, client, defaultViewParams(time.Minute))
	key2Query.View = view2
	ret3, err := client.GetRows(t.Context(), key2Query)
	require.NoError(t, err)
	// This is a new view, but it should be aggregated with the previous one.
	// So we expect the old version of item 2.
	requireResults(t, []*items{&testItem2}, ret3.Namespaces)

	env.endView(t, client, view0)
	env.endView(t, client, view1)
	env.endView(t, client, view2)

	view3 := env.beginView(t, client, defaultViewParams(time.Minute))
	key2Query.View = view3
	ret4, err := client.GetRows(t.Context(), key2Query)
	require.NoError(t, err)
	// After we cancelled the other views, a new view should create
	// a new transactions. So we expect the new version of item 2.
	requireResults(t, []*items{&testItem2Mod}, ret4.Namespaces)
	env.endView(t, client, view3)
}

func TestQueryPolicies(t *testing.T) {
	t.Parallel()
	env := newQueryServiceTestEnv(t, nil)

	policies, err := env.clientConn.GetNamespacePolicies(t.Context(), nil)
	require.NoError(t, err)
	require.NotNil(t, policies)
	require.Len(t, policies.Policies, len(env.ns))

	expectedNamespaces := make(map[string]any, len(env.ns))
	for _, ns := range env.ns {
		expectedNamespaces[ns] = nil
	}

	for _, p := range policies.Policies {
		_, ok := expectedNamespaces[p.Namespace]
		require.True(t, ok)
		delete(expectedNamespaces, p.Namespace)
		item, parseErr := policy.CreateNamespaceVerifier(p, nil)
		require.NoError(t, parseErr)
		require.NotNil(t, item)
		pol, parseErr := policy.UnmarshalNamespacePolicy(p.Policy)
		require.NoError(t, parseErr)
		rule := pol.GetThresholdRule()
		require.NotNil(t, rule)
		require.Equal(t, signature.Ecdsa, rule.Scheme)
	}

	configTX, err := env.clientConn.GetConfigTransaction(t.Context(), nil)
	require.NoError(t, err)
	require.NotNil(t, configTX)
	require.NotEmpty(t, configTX.Envelope)
}

func strToBytes(str ...string) [][]byte {
	ret := make([][]byte, len(str))
	for i, s := range str {
		ret[i] = encodeBytesForProto(s)
	}
	return ret
}

// encodeBytesForProto returns a byte array representation of a string such that when
// it will be serialized using [protojson.Format()], it will appear as the original string.
func encodeBytesForProto(str string) []byte {
	if len(str)%4 != 0 {
		str += strings.Repeat("+", 4-len(str)%4)
	}
	decodeString, err := base64.StdEncoding.DecodeString(str)
	if err != nil {
		panic(errors.Wrap(err, str))
	}
	return decodeString
}

func newQueryServiceTestEnv(t *testing.T, opts *queryServiceTestOpts) *queryServiceTestEnv {
	t.Helper()
	if opts == nil {
		opts = &queryServiceTestOpts{}
	}

	t.Log("generating config and namespaces")
	namespacesToTest := []string{"0", "1", "2"}
	dbConf := generateNamespacesUnderTest(t, namespacesToTest)

	config := &Config{
		MinBatchKeys:          5,
		MaxBatchWait:          time.Second,
		ViewAggregationWindow: time.Minute,
		MaxViewTimeout:        time.Minute,
		MaxAggregatedViews:    5,
		Server:                connection.NewLocalHostServer(opts.serverTLS),
		MaxRequestKeys:        opts.maxRequestKeys,
		Database:              dbConf,
		Monitoring: monitoring.Config{
			Server: connection.NewLocalHostServer(test.InsecureTLSConfig),
		},
	}

	qs := NewQueryService(config)
	test.RunServiceAndGrpcForTest(t.Context(), t, qs, qs.config.Server)
	clientConn := createQueryClientWithTLS(t, &qs.config.Server.Endpoint, opts.clientTLS)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)
	pool, err := vc.NewDatabasePool(ctx, config.Database)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	return &queryServiceTestEnv{
		config:     config,
		qs:         qs,
		ns:         namespacesToTest,
		clientConn: clientConn,
		pool:       pool,
	}
}

func generateNamespacesUnderTest(t *testing.T, namespaces []string) *vc.DatabaseConfig {
	t.Helper()
	env := vc.NewValidatorAndCommitServiceTestEnvWithTLS(t, 1, test.InsecureTLSConfig)
	env.SetupSystemTablesAndNamespaces(t.Context(), t)

	clientConf := loadgen.DefaultClientConf(t)
	clientConf.Adapter.VCClient = test.NewTLSMultiClientConfig(test.InsecureTLSConfig, env.Endpoints...)
	policies := workload.PolicyProfile{
		NamespacePolicies: make(map[string]*workload.Policy, len(namespaces)),
	}
	for i, ns := range append(namespaces, committerpb.MetaNamespaceID) {
		policies.NamespacePolicies[ns] = &workload.Policy{
			Scheme: signature.Ecdsa,
			Seed:   int64(i),
		}
	}
	clientConf.LoadProfile.Policy = policies
	clientConf.Generate = adapters.Phases{Config: true, Namespaces: true}
	client, err := loadgen.NewLoadGenClient(clientConf)
	require.NoError(t, err)
	err = client.Run(t.Context())
	require.NoError(t, connection.FilterStreamRPCError(err))
	return env.DBEnv.DBConf
}

type items struct {
	ns           string
	keys, values [][]byte
	versions     []uint64
}

func (it *items) asRows() []*committerpb.Row {
	rows := make([]*committerpb.Row, len(it.keys))
	for i, k := range it.keys {
		rows[i] = &committerpb.Row{
			Key:     k,
			Value:   it.values[i],
			Version: it.versions[i],
		}
	}
	return rows
}

func (it *items) asQuery() *committerpb.QueryNamespace {
	q := &committerpb.QueryNamespace{
		NsId: it.ns,
		// Add additional item that does not exist.
		Keys: append(it.keys, strToBytes("non+tx")...),
	}
	return q
}

func (q *queryServiceTestEnv) insert(t *testing.T, i *items) {
	t.Helper()
	require.NotEmpty(t, i.keys)
	require.Len(t, i.values, len(i.keys))
	require.Len(t, i.versions, len(i.keys))
	query := fmt.Sprintf(
		`insert into %s values (
			UNNEST($1::bytea[]), UNNEST($2::bytea[]), UNNEST($3::bigint[])
		);`,
		vc.TableName(i.ns),
	)
	_, err := q.pool.Exec(t.Context(), query, i.keys, i.values, i.versions)
	require.NoError(t, err)
}

func (q *queryServiceTestEnv) update(t *testing.T, i *items) {
	t.Helper()
	require.NotEmpty(t, i.keys)
	require.Len(t, i.values, len(i.keys))
	require.Len(t, i.versions, len(i.keys))
	query := fmt.Sprintf(`
		UPDATE %[1]s
			SET value = t.value,
				version = t.version
		FROM (
			SELECT * FROM UNNEST($1::bytea[], $2::bytea[], $3::bigint[]) AS t(key, value, version)
		) AS t
		WHERE %[1]s.key = t.key;
		`,
		vc.TableName(i.ns),
	)
	_, err := q.pool.Exec(t.Context(), query, i.keys, i.values, i.versions)
	require.NoError(t, err)
}

func requireResults(
	t *testing.T,
	expected []*items,
	ret []*committerpb.RowsNamespace,
) {
	t.Helper()
	require.Len(t, ret, len(expected))
	for i, item := range expected {
		requireRow(t, item, ret[i])
	}
}

func requireRow(
	t *testing.T,
	expected *items,
	ret *committerpb.RowsNamespace,
) {
	t.Helper()
	require.Equal(t, expected.ns, ret.NsId)
	test.RequireProtoElementsMatch(t, expected.asRows(), ret.Rows)
}

func requireIntVecMetricValue(t *testing.T, expected int, mv *prometheus.MetricVec, lvs ...string) {
	t.Helper()
	m, err := mv.GetMetricWithLabelValues(lvs...)
	require.NoError(t, err)
	test.RequireIntMetricValue(t, expected, m)
}

func (q *queryServiceTestEnv) beginView(
	t *testing.T,
	client committerpb.QueryServiceClient,
	params *committerpb.ViewParameters,
) *committerpb.View {
	t.Helper()
	view, err := client.BeginView(t.Context(), params)
	require.NoError(t, err)
	require.NotNil(t, view)
	require.NotEmpty(t, view.Id)
	_, file, line, _ := runtime.Caller(1)
	t.Cleanup(func() {
		if slices.Contains(q.disabledViews, view.Id) {
			return
		}
		//nolint:usetesting // t.Context() is dead at cleanup.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		t.Cleanup(cancel)
		_, err = client.EndView(ctx, view)
		require.NoErrorf(t, connection.FilterStreamRPCError(err), "view created in %s:%d", file, line)
	})
	return view
}

func (q *queryServiceTestEnv) endView(
	t *testing.T,
	client committerpb.QueryServiceClient,
	view *committerpb.View,
) {
	t.Helper()
	_, err := client.EndView(t.Context(), view)
	require.NoError(t, err)
	q.disabledViews = append(q.disabledViews, view.Id)
}

func (q *queryServiceTestEnv) makeItems(t *testing.T) []*items {
	t.Helper()
	requiredItems := make([]*items, len(q.ns))
	for i, ns := range q.ns {
		requiredItems[i] = &items{
			ns:       ns,
			keys:     strToBytes("item1", "item2", "item3", "item4"),
			values:   strToBytes("value1", "value2", "value3", "value4"),
			versions: []uint64{0, 1, 2, 3},
		}
		q.insert(t, requiredItems[i])
	}
	return requiredItems
}

func makeQuery(it []*items) (query *committerpb.Query, keyCount, querySize int) {
	query = &committerpb.Query{
		Namespaces: make([]*committerpb.QueryNamespace, len(it)),
	}
	for i, item := range it {
		keyCount += len(item.keys)
		qNs := item.asQuery()
		query.Namespaces[i] = qNs
		querySize += len(qNs.Keys)
	}
	return query, keyCount, querySize
}

func defaultViewParams(timeout time.Duration) *committerpb.ViewParameters {
	return &committerpb.ViewParameters{
		IsoLevel:            committerpb.IsoLevel_REPEATABLE_READ,
		NonDeferrable:       false,
		TimeoutMilliseconds: uint64(timeout.Milliseconds()), //nolint:gosec
	}
}

//nolint:ireturn // returning a gRPC client interface is intentional for test purpose.
func createQueryClientWithTLS(
	t *testing.T,
	ep *connection.Endpoint,
	tlsCfg connection.TLSConfig,
) committerpb.QueryServiceClient {
	t.Helper()
	return test.CreateClientWithTLS(t, ep, tlsCfg, committerpb.NewQueryServiceClient)
}
