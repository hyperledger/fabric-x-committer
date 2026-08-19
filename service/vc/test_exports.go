/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package vc

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yugabyte/pgx/v5"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/retry"
	"github.com/hyperledger/fabric-x-committer/utils/serve"
	"github.com/hyperledger/fabric-x-committer/utils/snapshotstate"
	"github.com/hyperledger/fabric-x-committer/utils/statedb"
	"github.com/hyperledger/fabric-x-committer/utils/test"
	"github.com/hyperledger/fabric-x-committer/utils/testdb"
)

const (
	queryKeyValueVersionSQLTmpt = "SELECT key, value, version FROM %s WHERE key = ANY($1)"
)

type (
	// ValidatorAndCommitterServiceTestEnv denotes the test environment for vcservice.
	ValidatorAndCommitterServiceTestEnv struct {
		VCServices    []*ValidatorCommitterService
		DBEnv         *DatabaseTestEnv
		Configs       []*Config
		ServerConfigs []*serve.Config
		Endpoints     []*connection.Endpoint
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
	serverConfigs := make([]*serve.Config, opts.NumServices)
	endpoints := make([]*connection.Endpoint, opts.NumServices)

	for i := range vcservices {
		config := &Config{
			Database:       opts.DBEnv.DBConf,
			ResourceLimits: opts.ResourceLimits,
		}
		serverConfig := test.NewLocalHostServiceConfig(opts.ServerCreds)
		vcs := NewValidatorCommitterService(initCtx, config)
		test.RunServiceAndServeForTest(t.Context(), t, vcs, serverConfig)
		vcservices[i] = vcs
		configs[i] = config
		serverConfigs[i] = serverConfig
		endpoints[i] = &serverConfig.GRPC.Endpoint
	}

	return &ValidatorAndCommitterServiceTestEnv{
		VCServices:    vcservices,
		DBEnv:         opts.DBEnv,
		Configs:       configs,
		ServerConfigs: serverConfigs,
		Endpoints:     endpoints,
	}
}

func defaultVCTestEnvOpts() *TestEnvOpts {
	return &TestEnvOpts{
		NumServices:    1,
		ServerCreds:    test.InsecureTLSConfig,
		ResourceLimits: defaultTestResourceLimits(),
	}
}

// defaultTestResourceLimits returns the resource limits used by all VC and database
// test environments.
func defaultTestResourceLimits() *ResourceLimitsConfig {
	return &ResourceLimitsConfig{
		MaxWorkersForPreparer:             2,
		MaxWorkersForValidator:            2,
		MaxWorkersForCommitter:            2,
		MinTransactionBatchSize:           1,
		TimeoutForMinTransactionBatchSize: 20 * time.Second,
	}
}

// GetDBEnv returns the database test environment.
func (vcEnv *ValidatorAndCommitterServiceTestEnv) GetDBEnv() *DatabaseTestEnv {
	if vcEnv == nil {
		return nil
	}
	return vcEnv.DBEnv
}

// DatabaseTestEnv represents a database test environment.
type DatabaseTestEnv struct {
	DB     *database
	DBConf *statedb.Config
}

// NewDatabaseTestEnv creates a new default database test environment.
func NewDatabaseTestEnv(t *testing.T) *DatabaseTestEnv {
	t.Helper()
	// default parameters set.
	return NewDatabaseTestEnvFromConnection(t, testdb.PrepareTestEnv(t), false)
}

// NewDatabaseTestEnvWithCustomConnection creates a new db test environment given a db connection.
func NewDatabaseTestEnvWithCustomConnection(t *testing.T, dbConnections *testdb.Connection) *DatabaseTestEnv {
	t.Helper()
	require.NotNil(t, dbConnections)
	return NewDatabaseTestEnvFromConnection(
		t, testdb.PrepareTestEnvWithConnection(t, dbConnections), dbConnections.LoadBalance,
	)
}

// NewDatabaseTestEnvFromConnection creates a new db test environment given a db connection without preparations.
func NewDatabaseTestEnvFromConnection(t *testing.T, cs *testdb.Connection, loadBalance bool) *DatabaseTestEnv {
	t.Helper()
	config := &statedb.Config{
		Endpoints:      cs.Endpoints,
		Username:       cs.User,
		Password:       cs.Password,
		Database:       cs.Database,
		MaxConnections: 10,
		MinConnections: 1,
		LoadBalance:    loadBalance,
		TLS:            cs.TLS,
		Retry:          testdb.DefaultRetry,
	}

	m := newVCServiceMetrics()
	sCtx, sCancel := context.WithTimeout(t.Context(), 5*time.Minute)
	t.Cleanup(sCancel)
	dbObject, err := newDatabase(sCtx, config, m)
	require.NoError(t, err, "%+v", err)
	t.Cleanup(dbObject.close)

	err = statedb.SetupSystemTablesAndNamespaces(sCtx, config)
	require.NoError(t, err, "failed to initialize database: %+v", err)

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
	count, err := retry.ExecuteWithResult(t.Context(), env.DB.retryProfile, func() (int, error) {
		var count int
		row := env.DB.pool.QueryRow(t.Context(), query)
		return count, row.Scan(&count)
	})
	require.NoError(t, err)
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
	batchStatus *servicepb.TxStatusBatch,
	txIDToHeight transactionIDToHeight,
) {
	t.Helper()
	newNsIDsWrites := namespaceToWrites{}
	for _, nsID := range createNsIDs {
		nsWrites := newNsIDsWrites.getOrCreate(committerpb.MetaNamespaceID)
		nsWrites.append([]byte(nsID), nil, 0)
	}

	require.NoError(t, retry.Execute(t.Context(), env.DB.retryProfile, func() error {
		res, err := env.DB.commit(t.Context(), &statesToBeCommitted{
			newWrites: newNsIDsWrites, batchStatus: batchStatus, txIDToHeight: txIDToHeight,
		})
		require.Nil(t, res)
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
		query := statedb.FmtNsID(insertQuery, nsID)
		require.NoError(t, retry.ExecuteSQL(
			t.Context(), env.DB.retryProfile, env.DB.pool, query,
			writes.keys, writes.values, writes.versions,
		))
	}
}

// FetchKeys fetches a list of keys.
func (env *DatabaseTestEnv) FetchKeys(t *testing.T, nsID string, keys [][]byte) map[string]*ValueVersion {
	t.Helper()
	query := fmt.Sprintf(queryKeyValueVersionSQLTmpt, statedb.TableName(nsID))

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
		"SELECT table_name FROM information_schema.tables WHERE table_name = '%s'", statedb.TableName(nsID),
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

// SnapshotFixture describes a durable `_snapshot` record a test wants in place.
// It exists because the snapshot service and the integration tests both need the
// state the commit path leaves behind, without driving a whole block through the
// pipeline: the record, the committed txID, the latest-snapshot pointer, and
// optionally the clone database itself.
type SnapshotFixture struct {
	Ref           *committerpb.TxRef
	Status        committerpb.SnapshotState_Status
	CloneDatabase string
	// CreateClone also creates the clone database named by CloneDatabase and
	// registers its cleanup. Leave it false for a record whose clone must not
	// exist (e.g. asserting how a missing clone is reported).
	CreateClone bool
}

// SnapshotRecord is a `_snapshot` record together with its row version, so a test
// can assert that a tick did NOT rewrite a record, not merely that its status is
// unchanged.
type SnapshotRecord struct {
	State   *committerpb.SnapshotState
	Version int64
}

// SeedSnapshotRecord commits a `_snapshot` record for f.Ref directly at f.Status,
// bypassing the normal PENDING-then-scheduler-advances flow, and returns the clone
// database name it recorded.
//
// Committing it through db.commit rather than a raw INSERT is what makes the
// fixture realistic: the txID lands in tx_status and the latest-snapshot pointer is
// written in the same transaction as the row, which is how a reader finds it.
func (env *DatabaseTestEnv) SeedSnapshotRecord(t *testing.T, f SnapshotFixture) string {
	t.Helper()
	if f.CreateClone {
		env.CreateSnapshotClone(t, f.CloneDatabase)
	}

	value, err := snapshotstate.Encode(&committerpb.SnapshotState{
		TxRef: f.Ref, Status: f.Status, CloneDatabase: f.CloneDatabase,
	})
	require.NoError(t, err)

	nws := make(namespaceToWrites)
	nws.getOrCreate(committerpb.SnapshotNamespaceID).append([]byte(f.Ref.TxId), value, 0)
	states := &statesToBeCommitted{
		newWrites: nws,
		batchStatus: &servicepb.TxStatusBatch{Status: []*committerpb.TxStatus{
			servicepb.NewHeightFromTxRef(f.Ref).WithStatus(f.Ref.TxId, committerpb.Status_COMMITTED),
		}},
		txIDToHeight: transactionIDToHeight{TxID(f.Ref.TxId): servicepb.NewHeightFromTxRef(f.Ref)},
	}

	// Retried exactly as the production commit path is: on PostgreSQL, creating a
	// clone runs pg_terminate_backend against the source database, which kills this
	// pool's connections, so a commit right after a clone can hit SQLSTATE 57P01
	// until the pool replaces them.
	_, err = retry.ExecuteWithResult(t.Context(), env.DB.retryProfile, func() (*commitResult, error) {
		return env.DB.commit(t.Context(), states)
	})
	require.NoError(t, err)
	return f.CloneDatabase
}

// SeedSnapshotRecordWithoutTxRef commits a `_snapshot` record whose value carries no
// TxRef, which a reader cannot address. Only a bug or storage corruption produces
// one, so a fixture is the only way to cover how a reader reports it.
func (env *DatabaseTestEnv) SeedSnapshotRecordWithoutTxRef(t *testing.T, txID, cloneDatabase string) {
	t.Helper()
	value, err := snapshotstate.Encode(&committerpb.SnapshotState{
		Status: committerpb.SnapshotState_PENDING, CloneDatabase: cloneDatabase,
	})
	require.NoError(t, err)

	nws := make(namespaceToWrites)
	nws.getOrCreate(committerpb.SnapshotNamespaceID).append([]byte(txID), value, 0)
	_, err = env.DB.commit(t.Context(), &statesToBeCommitted{newWrites: nws})
	require.NoError(t, err)
}

// CreateSnapshotClone creates the named clone database through the same path the
// commit uses, and registers its drop as cleanup so a parallel test run does not
// leak cluster-global databases.
func (env *DatabaseTestEnv) CreateSnapshotClone(t *testing.T, name string) {
	t.Helper()
	env.DropSnapshotCloneOnCleanup(t, name)
	require.NoError(t, env.DB.createSnapshotDatabase(t.Context(), name))
}

// DropSnapshotCloneOnCleanup drops the named clone database when the test ends, so a
// parallel run does not leak cluster-global databases.
func (env *DatabaseTestEnv) DropSnapshotCloneOnCleanup(t *testing.T, name string) {
	t.Helper()
	dropSnapshotCloneOnCleanup(t, env.DB, name)
}

// dropSnapshotCloneOnCleanup is the single clone-drop implementation, shared by the
// exported fixture above and by the in-package tests that hold a *database rather
// than a DatabaseTestEnv. It lives here, not in a _test.go file, so the exported
// fixture can reach it.
//
// The drop runs on context.Background() because the test context is already
// cancelled by the time cleanups run.
func dropSnapshotCloneOnCleanup(t *testing.T, db *database, name string) {
	t.Helper()
	t.Cleanup(func() {
		sql := fmt.Sprintf("DROP DATABASE IF EXISTS %s", pgx.Identifier{name}.Sanitize())
		_ = db.adminExec(context.Background(), sql)
	})
}

// ReadSnapshotRecord reads a `_snapshot` record and its row version.
//
// It goes through the retry profile rather than a bare query because creating a
// snapshot clone briefly severs this pool's connections on PostgreSQL, so an
// otherwise-correct read can fail purely because it raced a clone.
func (env *DatabaseTestEnv) ReadSnapshotRecord(ctx context.Context, txID string) (*SnapshotRecord, bool) {
	query := fmt.Sprintf("SELECT value, version FROM %s WHERE key = $1", snapshotstate.TableName())
	record, err := retry.ExecuteWithResult(ctx, env.DB.retryProfile, func() (*SnapshotRecord, error) {
		var raw []byte
		record := &SnapshotRecord{}
		if err := env.DB.pool.QueryRow(ctx, query, []byte(txID)).Scan(&raw, &record.Version); err != nil {
			return nil, err
		}
		state, err := snapshotstate.Decode(raw)
		record.State = state
		return record, err
	})
	if err != nil {
		return nil, false
	}
	return record, true
}

// SnapshotDatabaseName returns the deterministic clone-database name for ref, so a
// test outside this package names the same clone the commit path would.
func SnapshotDatabaseName(ref *committerpb.TxRef) string {
	return snapshotDatabaseName(ref)
}

// StateFixture describes committed state a test wants in the database before it
// exercises something that reads the whole state, such as hashing a clone.
type StateFixture struct {
	// NamespaceIDs are registered in ns__meta, which both creates their ns_<id>
	// tables and makes them part of the hashed table set.
	NamespaceIDs []string
	// Rows are the namespace rows to insert, keyed by namespace ID.
	Rows map[string][]KeyValue
	// TxStatuses are committed as COMMITTED tx_status rows, so a fixture can also
	// populate the tx_status table.
	TxStatuses []*committerpb.TxRef
}

// KeyValue is one namespace row in a StateFixture.
type KeyValue struct {
	Key   []byte
	Value []byte
}

// SeedState commits f through the normal commit path, so the resulting state is
// indistinguishable from state produced by real transactions.
func (env *DatabaseTestEnv) SeedState(t *testing.T, f StateFixture) {
	t.Helper()
	nsToWrites := namespaceToWrites{}
	for nsID, rows := range f.Rows {
		w := nsToWrites.getOrCreate(nsID)
		for _, row := range rows {
			w.append(row.Key, row.Value, 0)
		}
	}

	batchStatus := &servicepb.TxStatusBatch{}
	txIDToHeight := transactionIDToHeight{}
	for _, ref := range f.TxStatuses {
		batchStatus.Status = append(batchStatus.Status,
			committerpb.NewTxStatusFromRef(ref, committerpb.Status_COMMITTED))
		txIDToHeight[TxID(ref.TxId)] = servicepb.NewHeightFromTxRef(ref)
	}

	env.populateData(t, f.NamespaceIDs, nsToWrites, batchStatus, txIDToHeight)
}

// InsertRowDirectly inserts a row into ns_<nsID> without registering the namespace
// in ns__meta, which is the only way to populate a system namespace (`_snapshot`,
// `_checkpoint`) whose table already exists but is deliberately never registered.
func (env *DatabaseTestEnv) InsertRowDirectly(t *testing.T, nsID string, row KeyValue) {
	t.Helper()
	query := statedb.FmtNsID("INSERT INTO ns_${NAMESPACE_ID} (key, value, version) VALUES ($1, $2, 0)", nsID)
	require.NoError(t, retry.ExecuteSQL(
		t.Context(), env.DB.retryProfile, env.DB.pool, query, row.Key, row.Value,
	))
}
