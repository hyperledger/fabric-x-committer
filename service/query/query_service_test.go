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
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yugabyte/pgx/v4/pgxpool"
	"golang.org/x/exp/slices"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"

	"github.com/hyperledger/fabric-x-committer/api/protoqueryservice"
	"github.com/hyperledger/fabric-x-committer/api/types"
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

type queryServiceTestEnv struct {
	config        *Config
	qs            *Service
	ns            []string
	clientConn    *grpc.ClientConn
	pool          *pgxpool.Pool
	disabledViews []string
}

func TestQuery(t *testing.T) {
	t.Parallel()
	env := newQueryServiceTestEnv(t)
	requiredItems := env.makeItems(t)
	query, _, _ := makeQuery(requiredItems)

	for i, qNs := range query.Namespaces {
		expectedItem := requiredItems[i]
		qNs := qNs
		t.Run(fmt.Sprintf("Query internal NS %s", qNs.NsId), func(t *testing.T) {
			t.Parallel()
			ret, err := unsafeQueryRows(t.Context(), env.pool, qNs.NsId, qNs.Keys)
			require.NoError(t, err)
			requireRow(t, expectedItem, &protoqueryservice.RowsNamespace{
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
		client := protoqueryservice.NewQueryServiceClient(env.clientConn)
		ret, err := client.GetRows(t.Context(), query)
		require.NoError(t, err)
		requireResults(t, requiredItems, ret.Namespaces)
	})

	t.Run("Query GetRows client with view", func(t *testing.T) {
		t.Parallel()
		client := protoqueryservice.NewQueryServiceClient(env.clientConn)
		ret, err := client.GetRows(t.Context(), &protoqueryservice.Query{
			View:       env.beginView(t, client, defaultViewParams(time.Minute)),
			Namespaces: query.Namespaces,
		})
		require.NoError(t, err)
		requireResults(t, requiredItems, ret.Namespaces)
	})

	t.Run("Nil view parameters", func(t *testing.T) {
		t.Parallel()
		client := protoqueryservice.NewQueryServiceClient(env.clientConn)
		env.beginView(t, client, nil)
	})

	t.Run("Bad view ID", func(t *testing.T) {
		t.Parallel()
		client := protoqueryservice.NewQueryServiceClient(env.clientConn)
		_, err := client.EndView(t.Context(), &protoqueryservice.View{Id: "bad"})
		require.Equal(t, ErrInvalidOrStaleView.Error(), status.Convert(err).Message())
	})

	t.Run("Cancelled view ID", func(t *testing.T) {
		t.Parallel()
		client := protoqueryservice.NewQueryServiceClient(env.clientConn)
		view, err := client.BeginView(t.Context(), defaultViewParams(time.Minute))
		require.NoError(t, err)
		_, err = client.EndView(t.Context(), view)
		require.NoError(t, err)
		_, err = client.EndView(t.Context(), view)
		require.Equal(t, ErrInvalidOrStaleView.Error(), status.Convert(err).Message())
	})

	t.Run("Expired view ID", func(t *testing.T) {
		t.Parallel()
		client := protoqueryservice.NewQueryServiceClient(env.clientConn)
		view, err := client.BeginView(t.Context(), defaultViewParams(100*time.Millisecond))
		require.NoError(t, err)
		time.Sleep(150 * time.Millisecond)
		_, err = client.EndView(t.Context(), view)
		require.ErrorContains(t, err, status.Convert(err).Message())
	})
}

func TestQueryMetrics(t *testing.T) {
	t.Parallel()
	env := newQueryServiceTestEnv(t)
	requiredItems := env.makeItems(t)
	query, keyCount, querySize := makeQuery(requiredItems)

	client := protoqueryservice.NewQueryServiceClient(env.clientConn)

	t.Log("Query GetRows client with view")
	view0 := env.beginView(t, client, defaultViewParams(time.Minute))
	ret, err := client.GetRows(t.Context(), &protoqueryservice.Query{
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
	env.endView(t, client, view0)
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
	env := newQueryServiceTestEnv(t)
	requiredItems := env.makeItems(t)
	query, _, _ := makeQuery(requiredItems)

	client := protoqueryservice.NewQueryServiceClient(env.clientConn)

	t.Log("Query GetRows client with view")
	view0 := env.beginView(t, client, defaultViewParams(time.Minute))
	ret, err := client.GetRows(t.Context(), &protoqueryservice.Query{
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
	ret, err = client.GetRows(t.Context(), &protoqueryservice.Query{
		View: view1,
		Namespaces: []*protoqueryservice.QueryNamespace{{
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

	key2Query := &protoqueryservice.Query{
		View: view1,
		Namespaces: []*protoqueryservice.QueryNamespace{{
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
	env := newQueryServiceTestEnv(t)

	client := protoqueryservice.NewQueryServiceClient(env.clientConn)
	policies, err := client.GetNamespacePolicies(t.Context(), nil)
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
		item, parseErr := policy.ParseNamespacePolicyItem(p)
		require.NoError(t, parseErr)
		require.Equal(t, signature.Ecdsa, item.Scheme)
	}

	configTX, err := client.GetConfigTransaction(t.Context(), nil)
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

func newQueryServiceTestEnv(t *testing.T) *queryServiceTestEnv {
	t.Helper()
	t.Log("generating config and namespaces")
	namespacesToTest := []string{"0", "1", "2"}
	dbConf := generateNamespacesUnderTest(t, namespacesToTest)

	config := &Config{
		MinBatchKeys:          5,
		MaxBatchWait:          time.Second,
		ViewAggregationWindow: time.Minute,
		MaxViewTimeout:        time.Minute,
		MaxAggregatedViews:    5,
		Server: &connection.ServerConfig{
			Endpoint: connection.Endpoint{
				Host: "localhost",
				Port: 0,
			},
		},
		Database: dbConf,
		Monitoring: monitoring.Config{
			Server: connection.NewLocalHostServer(),
		},
	}

	qs := NewQueryService(config)
	sConfig := &connection.ServerConfig{
		Endpoint: connection.Endpoint{Host: "localhost", Port: 0},
	}
	test.RunServiceAndGrpcForTest(t.Context(), t, qs, sConfig, func(server *grpc.Server) {
		protoqueryservice.RegisterQueryServiceServer(server, qs)
	})

	clientConn, err := connection.Connect(connection.NewInsecureDialConfig(&sConfig.Endpoint))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, clientConn.Close())
	})

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
	env := vc.NewValidatorAndCommitServiceTestEnv(t, 1)
	env.SetupSystemTablesAndNamespaces(t.Context(), t)

	clientConf := loadgen.DefaultClientConf()
	clientConf.Adapter.VCClient = &adapters.VCClientConfig{
		Endpoints: env.Endpoints,
	}
	policies := &workload.PolicyProfile{
		NamespacePolicies: make(map[string]*workload.Policy, len(namespaces)),
	}
	for i, ns := range append(namespaces, types.MetaNamespaceID) {
		policies.NamespacePolicies[ns] = &workload.Policy{
			Scheme: signature.Ecdsa,
			Seed:   int64(i),
		}
	}
	clientConf.LoadProfile.Transaction.Policy = policies
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

func (it *items) asRows() []*protoqueryservice.Row {
	rows := make([]*protoqueryservice.Row, len(it.keys))
	for i, k := range it.keys {
		rows[i] = &protoqueryservice.Row{
			Key:     k,
			Value:   it.values[i],
			Version: it.versions[i],
		}
	}
	return rows
}

func (it *items) asQuery() *protoqueryservice.QueryNamespace {
	q := &protoqueryservice.QueryNamespace{
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
	ret []*protoqueryservice.RowsNamespace,
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
	ret *protoqueryservice.RowsNamespace,
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
	client protoqueryservice.QueryServiceClient,
	params *protoqueryservice.ViewParameters,
) *protoqueryservice.View {
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
	client protoqueryservice.QueryServiceClient,
	view *protoqueryservice.View,
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

func makeQuery(it []*items) (query *protoqueryservice.Query, keyCount, querySize int) {
	query = &protoqueryservice.Query{
		Namespaces: make([]*protoqueryservice.QueryNamespace, len(it)),
	}
	for i, item := range it {
		keyCount += len(item.keys)
		qNs := item.asQuery()
		query.Namespaces[i] = qNs
		querySize += len(qNs.Keys)
	}
	return query, keyCount, querySize
}

func defaultViewParams(timeout time.Duration) *protoqueryservice.ViewParameters {
	return &protoqueryservice.ViewParameters{
		IsoLevel:            protoqueryservice.IsoLevel_RepeatableRead,
		NonDeferrable:       false,
		TimeoutMilliseconds: uint64(timeout.Milliseconds()), //nolint:gosec
	}
}
