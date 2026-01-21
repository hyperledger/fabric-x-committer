/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package vc

import (
	"context"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/service/vc/dbtest"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

const (
	queryKeyValueVersionSQLTmpt = "SELECT key, value, version FROM %s WHERE key = ANY($1)"
)

type (
	// ValidatorAndCommitterServiceTestEnv denotes the test environment for vcservice.
	ValidatorAndCommitterServiceTestEnv struct {
		VCServices []*ValidatorCommitterService
		DBEnv      *DatabaseTestEnv
		Configs    []*Config
		Endpoints  []*connection.Endpoint
	}

	// ValueVersion contains a list of values and their matching versions.
	ValueVersion struct {
		Value   []byte
		Version uint64
	}

	// TestEnvOpts contains options for creating a VC test environment.
	TestEnvOpts struct {
		NumServices    int
		ServerCreds    connection.TLSConfig
		ResourceLimits *ResourceLimitsConfig
		DBEnv          *DatabaseTestEnv
	}
)

// NewValidatorAndCommitServiceTestEnv creates a new test environment with the given options.
func NewValidatorAndCommitServiceTestEnv(t *testing.T, opts *TestEnvOpts) *ValidatorAndCommitterServiceTestEnv {
	t.Helper()

	if opts == nil {
		opts = defaultVCTestEnvOpts()
	}

	if opts.NumServices == 0 {
		opts.NumServices = 1
	}

	if opts.ResourceLimits == nil {
		opts.ResourceLimits = defaultVCTestEnvOpts().ResourceLimits
	}

	if opts.DBEnv == nil {
		opts.DBEnv = NewDatabaseTestEnv(t)
	}

	initCtx, initCancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(initCancel)

	vcservices := make([]*ValidatorCommitterService, opts.NumServices)
	configs := make([]*Config, opts.NumServices)
	endpoints := make([]*connection.Endpoint, opts.NumServices)

	for i := range vcservices {
		config := &Config{
			Server:         connection.NewLocalHostServer(opts.ServerCreds),
			Database:       opts.DBEnv.DBConf,
			ResourceLimits: opts.ResourceLimits,
			Monitoring: monitoring.Config{
				Server: connection.NewLocalHostServer(test.InsecureTLSConfig),
			},
		}
		vcs, err := NewValidatorCommitterService(initCtx, config)
		require.NoError(t, err)
		t.Cleanup(vcs.Close)
		test.RunServiceAndGrpcForTest(t.Context(), t, vcs, config.Server)
		vcservices[i] = vcs
		configs[i] = config
		endpoints[i] = &config.Server.Endpoint
	}

	return &ValidatorAndCommitterServiceTestEnv{
		VCServices: vcservices,
		DBEnv:      opts.DBEnv,
		Configs:    configs,
		Endpoints:  endpoints,
	}
}

func defaultVCTestEnvOpts() *TestEnvOpts {
	return &TestEnvOpts{
		NumServices: 1,
		ServerCreds: test.InsecureTLSConfig,
		ResourceLimits: &ResourceLimitsConfig{
			MaxWorkersForPreparer:             2,
			MaxWorkersForValidator:            2,
			MaxWorkersForCommitter:            2,
			MinTransactionBatchSize:           1,
			TimeoutForMinTransactionBatchSize: 20 * time.Second,
		},
	}
}

// GetDBEnv returns the database test environment.
func (vcEnv *ValidatorAndCommitterServiceTestEnv) GetDBEnv() *DatabaseTestEnv {
	if vcEnv == nil {
		return nil
	}
	return vcEnv.DBEnv
}

// SetupSystemTablesAndNamespaces creates the required system tables and namespaces.
func (vcEnv *ValidatorAndCommitterServiceTestEnv) SetupSystemTablesAndNamespaces(ctx context.Context, t *testing.T) {
	t.Helper()
	require.NoError(t, vcEnv.DBEnv.DB.setupSystemTablesAndNamespaces(ctx))
}

// DatabaseTestEnv represents a database test environment.
type DatabaseTestEnv struct {
	DB     *database
	DBConf *DatabaseConfig
}

// NewDatabaseTestEnv creates a new default database test environment.
func NewDatabaseTestEnv(t *testing.T) *DatabaseTestEnv {
	t.Helper()
	// default parameters set.
	return NewDatabaseTestEnvFromConnection(t, dbtest.PrepareTestEnv(t), false)
}

// NewDatabaseTestEnvWithCustomConnection creates a new db test environment given a db connection.
func NewDatabaseTestEnvWithCustomConnection(t *testing.T, dbConnections *dbtest.Connection) *DatabaseTestEnv {
	t.Helper()
	require.NotNil(t, dbConnections)
	return NewDatabaseTestEnvFromConnection(
		t, dbtest.PrepareTestEnvWithConnection(t, dbConnections), dbConnections.LoadBalance,
	)
}

// NewDatabaseTestEnvFromConnection creates a new db test environment given a db connection without preparations.
func NewDatabaseTestEnvFromConnection(t *testing.T, cs *dbtest.Connection, loadBalance bool) *DatabaseTestEnv {
	t.Helper()
	config := &DatabaseConfig{
		Endpoints:      cs.Endpoints,
		Username:       cs.User,
		Password:       cs.Password,
		Database:       cs.Database,
		MaxConnections: 10,
		MinConnections: 1,
		LoadBalance:    loadBalance,
		TLS:            cs.TLS,
		Retry: &connection.RetryProfile{
			MaxElapsedTime:  5 * time.Minute,
			InitialInterval: time.Duration(rand.Intn(900)+100) * time.Millisecond,
		},
	}

	m := newVCServiceMetrics()
	sCtx, sCancel := context.WithTimeout(t.Context(), 5*time.Minute)
	t.Cleanup(sCancel)
	dbObject, err := newDatabase(sCtx, config, m)
	require.NoError(t, err, "%+v", err)
	t.Cleanup(dbObject.close)

	return &DatabaseTestEnv{
		DB:     dbObject,
		DBConf: config,
	}
}

// CountStatus returns the number of transactions with a given tx status.
func (env *DatabaseTestEnv) CountStatus(t *testing.T, status committerpb.Status) int {
	t.Helper()
	return env.getRowCount(t, fmt.Sprintf("SELECT count(*) FROM tx_status WHERE status = %d", status))
}

// CountAlternateStatus returns the number of transactions not with a given tx status.
func (env *DatabaseTestEnv) CountAlternateStatus(t *testing.T, status committerpb.Status) int {
	t.Helper()
	return env.getRowCount(t, fmt.Sprintf("SELECT count(*) FROM tx_status WHERE status != %d", status))
}

// queryRow execute a single-row query and return the result.
func (env *DatabaseTestEnv) getRowCount(t *testing.T, query string) int {
	t.Helper()
	var count int
	require.NoError(t, env.DB.retry.Execute(t.Context(), func() error {
		row := env.DB.pool.QueryRow(t.Context(), query)
		return row.Scan(&count)
	}))

	return count
}

// StatusExistsForNonDuplicateTxID ensures that the given statuses and height
// exist for the corresponding txIDs in the tx_status table, excluding any
// duplicate txID statuses.
func (env *DatabaseTestEnv) StatusExistsForNonDuplicateTxID(
	ctx context.Context,
	t *testing.T,
	expectedStatuses []*committerpb.TxStatus,
) {
	t.Helper()
	persistedTxIDs := make([][]byte, 0, len(expectedStatuses))
	persistedExpectedStatuses := make([]*committerpb.TxStatus, 0, len(expectedStatuses))
	for _, s := range expectedStatuses {
		if s.Status < committerpb.Status_REJECTED_DUPLICATE_TX_ID {
			persistedTxIDs = append(persistedTxIDs, []byte(s.Ref.TxId))
			persistedExpectedStatuses = append(persistedExpectedStatuses, s)
		}
	}

	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	actualStatuses, err := env.DB.readStatusWithHeight(ctx, persistedTxIDs)
	require.NoError(t, err)
	test.RequireProtoElementsMatch(t, persistedExpectedStatuses, actualStatuses)
}

// StatusExistsWithDifferentHeightForDuplicateTxID ensures that the given
// statuses and height do not exist for corresponding txIDs in the tx_status
// table for duplicate txID statuses.
func (env *DatabaseTestEnv) StatusExistsWithDifferentHeightForDuplicateTxID(
	t *testing.T,
	expectedStatuses []*committerpb.TxStatus,
) {
	t.Helper()
	txIDs := make([][]byte, 0, len(expectedStatuses))
	for _, s := range expectedStatuses {
		txIDs = append(txIDs, []byte(s.Ref.TxId))
	}

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	actualTxStatuses, err := env.DB.readStatusWithHeight(ctx, txIDs)
	require.NoError(t, err)
	actualTxStatusMap := make(map[string]*committerpb.TxStatus, len(actualTxStatuses))
	for _, txStatus := range actualTxStatuses {
		actualTxStatusMap[txStatus.Ref.TxId] = txStatus
	}

	require.Len(t, actualTxStatuses, len(expectedStatuses))
	for _, s := range expectedStatuses {
		// For the duplicate txID, neither the status nor the height would match the entry in the
		// transaction status table.
		txID := s.Ref.TxId
		require.NotEqual(t, s.Status, actualTxStatusMap[txID].Status)
		expHeight := servicepb.NewHeightFromTxRef(s.Ref)
		actualHeight := servicepb.NewHeightFromTxRef(actualTxStatusMap[txID].Ref)
		require.NotEqual(t, expHeight, actualHeight)
	}
}

func (env *DatabaseTestEnv) populateData( //nolint:revive
	t *testing.T,
	createNsIDs []string,
	nsToWrites namespaceToWrites,
	batchStatus *committerpb.TxStatusBatch,
	txIDToHeight transactionIDToHeight,
) {
	t.Helper()
	newNsIDsWrites := namespaceToWrites{}
	for _, nsID := range createNsIDs {
		nsWrites := newNsIDsWrites.getOrCreate(committerpb.MetaNamespaceID)
		nsWrites.append([]byte(nsID), nil, 0)
	}

	require.NoError(t, env.DB.retry.Execute(t.Context(), func() error {
		conflicts, duplicate, err := env.DB.commit(t.Context(), &statesToBeCommitted{
			newWrites: newNsIDsWrites, batchStatus: batchStatus, txIDToHeight: txIDToHeight,
		})
		require.Empty(t, conflicts)
		require.Empty(t, duplicate)
		logger.WarnStackTrace(err)
		return err
	}))

	for nsID, writes := range nsToWrites {
		if writes.empty() {
			continue
		}

		require.NotNil(t, writes.keys)
		require.NotNil(t, writes.values)
		require.NotNil(t, writes.versions)
		require.Len(t, writes.values, len(writes.keys))
		require.Len(t, writes.versions, len(writes.keys))

		insertQuery := `
INSERT INTO ns_${NAMESPACE_ID} (key, value, version)
SELECT _key, _value, _version
FROM UNNEST($1::bytea[], $2::bytea[], $3::bigint[]) AS t(_key, _value, _version);
`
		query := FmtNsID(insertQuery, nsID)
		require.NoError(t, env.DB.retry.ExecuteSQL(t.Context(), env.DB.pool, query,
			writes.keys, writes.values, writes.versions,
		))
	}
}

// FetchKeys fetches a list of keys.
func (env *DatabaseTestEnv) FetchKeys(t *testing.T, nsID string, keys [][]byte) map[string]*ValueVersion {
	t.Helper()
	query := fmt.Sprintf(queryKeyValueVersionSQLTmpt, TableName(nsID))

	kvPairs, err := env.DB.pool.Query(t.Context(), query, keys)
	require.NoError(t, err)
	defer kvPairs.Close()

	actualRows := map[string]*ValueVersion{}

	for kvPairs.Next() {
		var key []byte
		vv := &ValueVersion{}

		require.NoError(t, kvPairs.Scan(&key, &vv.Value, &vv.Version))
		actualRows[string(key)] = vv
	}

	require.NoError(t, kvPairs.Err())

	return actualRows
}

func (env *DatabaseTestEnv) tableExists(t *testing.T, nsID string) {
	t.Helper()
	query := fmt.Sprintf(
		"SELECT table_name FROM information_schema.tables WHERE table_name = '%s'", TableName(nsID),
	)
	names, err := env.DB.pool.Query(t.Context(), query)
	require.NoError(t, err)
	defer names.Close()
	require.True(t, names.Next())
}

func (env *DatabaseTestEnv) rowExists(t *testing.T, nsID string, exp namespaceWrites) {
	t.Helper()
	actualRows := env.FetchKeys(t, nsID, exp.keys)

	assert.Len(t, actualRows, len(exp.keys))
	for i, key := range exp.keys {
		if assert.NotNil(t, actualRows[string(key)], "key: %s", string(key)) {
			assert.Equal(t, exp.values[i], actualRows[string(key)].Value, "key: %s", string(key))
			assert.EqualExportedValuesf(t, exp.versions[i], actualRows[string(key)].Version, "key: %s", string(key))
		}
	}
}

func (env *DatabaseTestEnv) rowNotExists(t *testing.T, nsID string, keys [][]byte) {
	t.Helper()
	actualRows := env.FetchKeys(t, nsID, keys)
	assert.Empty(t, actualRows)
	for key, valVer := range actualRows {
		assert.Failf(t, "Key should not exist", "key [%s] value: [%s] version [%d]",
			key, string(valVer.Value), valVer.Version)
	}
}
