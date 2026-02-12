/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package vc

import (
	"strings"
	"testing"

	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/require"
	"github.com/yugabyte/pgx/v4/pgxpool"
)

const (
	ns1 = "1"
	ns2 = "2"
)

func newDatabaseTestEnvWithTablesSetup(t *testing.T) *DatabaseTestEnv {
	t.Helper()
	env := NewDatabaseTestEnv(t)
	ctx, _ := createContext(t)
	require.NoError(t, env.DB.setupSystemTablesAndNamespaces(ctx))
	return env
}

func TestTablesAndMethods(t *testing.T) {
	t.Parallel()
	env := NewDatabaseTestEnv(t)
	ctx, _ := createContext(t)
	require.NoError(t, env.DB.setupSystemTablesAndNamespaces(ctx))
	env.populateData(t, []string{"a", "b"}, namespaceToWrites{}, nil, nil)

	expectedTables := []string{"metadata", "tx_status", "ns__meta", "ns__config", "ns_a", "ns_b"}
	expectedMethodsPerNamespace := []string{"insert_", "update_", "validate_reads_"}
	expectedMethods := []string{"insert_tx_status"}
	for _, table := range expectedTables {
		if !strings.HasPrefix(table, "ns_") {
			continue
		}
		for _, method := range expectedMethodsPerNamespace {
			expectedMethods = append(expectedMethods, method+table)
		}
	}

	tables := readTables(t, env.DB.pool)
	require.ElementsMatch(t, tables, expectedTables)
	methods := readMethods(t, env.DB.pool)
	require.ElementsMatch(t, methods, expectedMethods)
}

func readTables(t *testing.T, querier *pgxpool.Pool) []string {
	t.Helper()
	rows, err := querier.Query(t.Context(), `
SELECT DISTINCT tablename
FROM pg_catalog.pg_tables
WHERE tablename NOT LIKE 'sql_%'
  AND tablename NOT LIKE 'pg_%';
`)
	require.NoError(t, err)
	t.Cleanup(rows.Close)

	var tables []string
	for rows.Next() {
		require.NoError(t, rows.Err())
		var tableName string
		require.NoError(t, rows.Scan(&tableName))
		t.Logf("table: %s", tableName)
		tables = append(tables, tableName)
	}
	return tables
}

func readMethods(t *testing.T, querier *pgxpool.Pool) []string {
	t.Helper()
	rows, err := querier.Query(t.Context(), `
SELECT proname, pg_get_function_identity_arguments(oid)
FROM pg_proc
WHERE pronamespace = 'public'::regnamespace
  AND proname NOT LIKE 'sql_%'
  AND proname NOT LIKE 'pg_%';
`)
	require.NoError(t, err)
	defer rows.Close()

	var methods []string
	for rows.Next() {
		require.NoError(t, rows.Err())
		var methodName string
		var methodVars string
		require.NoError(t, rows.Scan(&methodName, &methodVars))
		t.Logf("method: %-30s vars: %s", methodName, methodVars)
		methods = append(methods, methodName)
	}
	return methods
}

func TestValidateNamespaceReads(t *testing.T) {
	t.Parallel()
	env := newDatabaseTestEnvWithTablesSetup(t)

	k1 := []byte("key1")
	k2 := []byte("key2")
	k3 := []byte("key3")
	k4 := []byte("key4")
	k5 := []byte("key5")
	k6 := []byte("key6")
	k7 := []byte("key7")
	k8 := []byte("key8")
	k9 := []byte("key9")

	env.populateData(
		t,
		[]string{ns1, ns2},
		namespaceToWrites{
			ns1: {
				keys:     [][]byte{k1, k2, k3},
				values:   [][]byte{[]byte("value1"), []byte("value2"), []byte("value3")},
				versions: []uint64{0, 0, 0},
			},
			ns2: {
				keys:     [][]byte{k4, k5, k6},
				values:   [][]byte{[]byte("value4"), []byte("value5"), []byte("value6")},
				versions: []uint64{1, 1, 1},
			},
		},
		nil,
		nil,
	)

	// Shortcut for version pointer.
	v := applicationpb.NewVersion

	tests := []struct {
		name                  string
		nsID                  string
		r                     *reads
		expectedReadConflicts *reads
	}{
		{
			name:                  "empty reads",
			nsID:                  ns1,
			r:                     &reads{},
			expectedReadConflicts: &reads{},
		},
		{
			name: "reads of only non-existing keys and all matching versions",
			nsID: ns1,
			r: &reads{
				keys:     [][]byte{k4, k5, k6},
				versions: []*uint64{nil, nil, nil},
			},
			expectedReadConflicts: &reads{},
		},
		{
			name: "reads of only non-existing keys and some mismatching versions",
			nsID: ns2,
			r: &reads{
				keys:     [][]byte{k7, k8, k9},
				versions: []*uint64{nil, v(0), nil},
			},
			expectedReadConflicts: &reads{
				keys:     [][]byte{k8},
				versions: []*uint64{v(0)},
			},
		},
		{
			name: "reads of only non-existing keys and all mismatching versions",
			nsID: ns2,
			r: &reads{
				keys:     [][]byte{k7, k8, k9},
				versions: []*uint64{v(1), v(0), v(1)},
			},
			expectedReadConflicts: &reads{
				keys:     [][]byte{k7, k8, k9},
				versions: []*uint64{v(1), v(0), v(1)},
			},
		},
		{
			name: "reads of existing keys and all matching versions",
			nsID: ns1,
			r: &reads{
				keys:     [][]byte{k1, k2, k3},
				versions: []*uint64{v(0), v(0), v(0)},
			},
			expectedReadConflicts: &reads{},
		},
		{
			name: "reads of existing keys and some mismatching versions",
			nsID: ns1,
			r: &reads{
				keys:     [][]byte{k1, k2, k3},
				versions: []*uint64{v(1), v(0), v(1)},
			},
			expectedReadConflicts: &reads{
				keys:     [][]byte{k1, k3},
				versions: []*uint64{v(1), v(1)},
			},
		},
		{
			name: "reads of existing keys and all mismatching versions",
			nsID: ns2,
			r: &reads{
				keys:     [][]byte{k4, k5, k6},
				versions: []*uint64{v(0), v(0), v(0)},
			},
			expectedReadConflicts: &reads{
				keys:     [][]byte{k4, k5, k6},
				versions: []*uint64{v(0), v(0), v(0)},
			},
		},
		{
			name: "reads of existing and non-existing keys and some mismatching versions",
			nsID: ns2,
			r: &reads{
				keys:     [][]byte{k4, k5, k6, k7, k8, k9},
				versions: []*uint64{v(1), v(0), v(1), nil, v(0), nil},
			},
			expectedReadConflicts: &reads{
				keys:     [][]byte{k5, k8},
				versions: []*uint64{v(0), v(0)},
			},
		},
	}

	for _, test := range tests {
		tt := test
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			readConflicts, err := env.DB.validateNamespaceReads(t.Context(), tt.nsID, tt.r)
			require.NoError(t, err)
			require.ElementsMatch(t, toComparableReads(tt.expectedReadConflicts), toComparableReads(readConflicts))
		})
	}
}

func TestDBCommit(t *testing.T) {
	t.Parallel()
	dbEnv := newDatabaseTestEnvWithTablesSetup(t)

	require.Equal(t, dbEnv.DBConf.Retry, dbEnv.DB.retry)

	dbEnv.populateData(
		t,
		[]string{ns1, ns2},
		nil,
		nil,
		nil,
	)

	k1 := []byte("key1")
	k2 := []byte("key2")
	k3 := []byte("key3")
	k4 := []byte("key4")
	k5 := []byte("key5")
	k6 := []byte("key6")

	nsToWrites := namespaceToWrites{
		ns1: {
			keys:     [][]byte{k1, k2, k3},
			values:   [][]byte{[]byte("value1"), []byte("value2"), []byte("value3")},
			versions: []uint64{0, 0, 0},
		},
		ns2: {
			keys:     [][]byte{k4, k5, k6},
			values:   [][]byte{[]byte("value4"), []byte("value5"), []byte("value6")},
			versions: []uint64{1, 1, 1},
		},
		committerpb.MetaNamespaceID: {
			keys:     [][]byte{[]byte("3"), []byte("4")},
			values:   [][]byte{[]byte("value7"), []byte("value8")},
			versions: []uint64{0, 0, 0},
		},
	}

	commit(t, dbEnv, &statesToBeCommitted{newWrites: nsToWrites})
	commit(t, dbEnv, &statesToBeCommitted{updateWrites: nsToWrites})
	dbEnv.rowExists(t, ns1, *nsToWrites[ns1])
	dbEnv.rowExists(t, ns2, *nsToWrites[ns2])
	dbEnv.rowExists(t, committerpb.MetaNamespaceID, *nsToWrites[committerpb.MetaNamespaceID])
	dbEnv.tableExists(t, "3")
	dbEnv.tableExists(t, "4")

	nsToWrites = namespaceToWrites{
		"3": {
			keys:     [][]byte{k1, k2},
			values:   [][]byte{[]byte("value1"), []byte("value2")},
			versions: []uint64{0, 0},
		},
		"4": {
			keys:     [][]byte{k4, k5},
			values:   [][]byte{[]byte("value4"), []byte("value5")},
			versions: []uint64{0, 0},
		},
	}

	commit(t, dbEnv, &statesToBeCommitted{newWrites: nsToWrites})
	dbEnv.rowExists(t, "3", *nsToWrites["3"])
	dbEnv.rowExists(t, "4", *nsToWrites["4"])
}

func TestNewDatabaseTabletsWithDetection(t *testing.T) {
	t.Parallel()
	env := NewDatabaseTestEnv(t)

	// Detect the actual DB type to know what to assert.
	actuallyYugabyte, err := isYugabyteDB(t.Context(), env.DB.pool)
	require.NoError(t, err)

	env.DBConf.TablePreSplitTablets = 5

	db, err := newDatabase(t.Context(), env.DBConf, newVCServiceMetrics())
	require.NoError(t, err)
	t.Cleanup(db.close)

	expectedTablePreSplitTablets := env.DBConf.TablePreSplitTablets
	if !actuallyYugabyte {
		expectedTablePreSplitTablets = 0
	}
	require.Equal(t, expectedTablePreSplitTablets, db.tablePreSplitTablets)
}

func commit(t *testing.T, dbEnv *DatabaseTestEnv, states *statesToBeCommitted) {
	t.Helper()
	require.NoError(t, dbEnv.DBConf.Retry.Execute(t.Context(), func() error {
		_, _, err := dbEnv.DB.commit(t.Context(), states)
		return err
	}))
}

func toComparableReads(r *reads) []comparableRead {
	if r == nil || len(r.keys) == 0 {
		return nil
	}
	compReads := make([]comparableRead, len(r.keys))
	for i := range r.keys {
		compReads[i] = newCmpRead("", r.keys[i], r.versions[i])
	}
	return compReads
}
