/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package vc

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/hyperledger/fabric-x-common/common/policydsl"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	verifierpolicy "github.com/hyperledger/fabric-x-committer/service/verifier/policy"
	"github.com/hyperledger/fabric-x-committer/utils/signature"
	"github.com/hyperledger/fabric-x-committer/utils/statedb"
	"github.com/hyperledger/fabric-x-committer/utils/testsig"
	"github.com/syndbg/fabric-x-migrate-poc/pkg/genesisdata"
)

const migrationNamespace = "migration_basic"

func TestInitFromSnapshot(t *testing.T) {
	t.Run("when the target is empty, it imports and verifies the snapshot", testInitFromSnapshot)
	testInitFromSnapshotRejectsUnsafeTargets(t)
	t.Run("when the final migration record fails, it rolls back every imported row", testInitFromSnapshotAtomic)
	t.Run("when multiple namespaces are mapped, it imports each namespace", testInitFromSnapshotMultipleNamespaces)
}

func testInitFromSnapshot(t *testing.T) {
	env := newSnapshotBootstrapEnv(t, true)
	file := writeSnapshotBundle(t, migrationNamespace, "value-a")

	result, err := env.DB.initFromSnapshot(t.Context(), file)
	require.NoError(t, err)
	assert.False(t, result.AlreadyImported)
	assert.Equal(t, uint64(1), result.TargetAnchor)
	assert.Equal(t, uint64(2), result.PublicStateCount)
	assert.Equal(t, uint64(2), result.TransactionIDs)

	var value []byte
	var version int64
	require.NoError(t, env.DB.pool.QueryRow(t.Context(),
		"SELECT value, version FROM ns_migration_basic WHERE key = $1", []byte("asset-a"),
	).Scan(&value, &version))
	assert.Equal(t, []byte("value-a"), value)
	assert.Zero(t, version)
	assert.Equal(t, 2, env.getRowCount(t, "SELECT count(*) FROM migrated_tx_ids"))
	assert.Equal(t, 1, env.getRowCount(t, "SELECT count(*) FROM migration_record WHERE status = 'VERIFIED'"))

	anchor, err := env.DB.getNextBlockNumberToCommit(t.Context())
	require.NoError(t, err)
	assert.Equal(t, uint64(2), anchor.Number, "bootstrap must not advance target block 1")

	runAgain, err := env.DB.initFromSnapshot(t.Context(), file)
	require.NoError(t, err)
	assert.True(t, runAgain.AlreadyImported)
	assert.Equal(t, 2, env.getRowCount(t, "SELECT count(*) FROM ns_migration_basic"))

	row := env.DB.pool.QueryRow(t.Context(), insertTxStatusSQLStmt,
		[][]byte{[]byte("tx-a")}, []int{int(committerpb.Status_COMMITTED)}, [][]byte{make([]byte, 16)})
	duplicates, err := readArrayResult[[]byte](row)
	require.NoError(t, err)
	assert.Equal(t, [][]byte{[]byte("tx-a")}, duplicates, "migrated transaction IDs must participate in anti-replay")
	assert.Zero(t, env.getRowCount(t, "SELECT count(*) FROM tx_status"))

	different := writeSnapshotBundle(t, migrationNamespace, "different-value")
	_, err = env.DB.initFromSnapshot(t.Context(), different)
	require.ErrorContains(t, err, "different genesis-data file")

	_, err = env.DB.pool.Exec(t.Context(), "UPDATE ns_migration_basic SET value = $1 WHERE key = $2", []byte("tampered"), []byte("asset-a"))
	require.NoError(t, err)
	_, err = env.DB.initFromSnapshot(t.Context(), file)
	require.ErrorContains(t, err, "target public state does not match")
	_, err = env.DB.pool.Exec(t.Context(), "UPDATE ns_migration_basic SET value = $1 WHERE key = $2", []byte("value-a"), []byte("asset-a"))
	require.NoError(t, err)

	_, err = env.DB.pool.Exec(t.Context(), "UPDATE ns__meta SET value = $1 WHERE key = $2", mspPolicyBytes(t, "OR('Org1MSP.admin')"), []byte(migrationNamespace))
	require.NoError(t, err)
	_, err = env.DB.initFromSnapshot(t.Context(), file)
	require.ErrorContains(t, err, "target bindings")
	_, err = env.DB.pool.Exec(t.Context(), "UPDATE ns__meta SET value = $1 WHERE key = $2", mspPolicyBytes(t, "OR('Org1MSP.member')"), []byte(migrationNamespace))
	require.NoError(t, err)

	require.NoError(t, env.DB.setLastCommittedBlockNumber(t.Context(), &servicepb.BlockRef{Number: 2}))
	_, err = env.DB.initFromSnapshot(t.Context(), file)
	require.ErrorContains(t, err, "advanced beyond")
}

func testInitFromSnapshotRejectsUnsafeTargets(t *testing.T) {
	t.Run("when the target namespace is missing, it rejects the import", func(t *testing.T) {
		env := newSnapshotBootstrapEnv(t, false)
		_, err := env.DB.initFromSnapshot(t.Context(), writeSnapshotBundle(t, migrationNamespace, "value-a"))
		require.ErrorContains(t, err, "is not installed")
	})

	t.Run("when the target namespace is not empty, it rejects the import", func(t *testing.T) {
		env := newSnapshotBootstrapEnv(t, true)
		_, err := env.DB.pool.Exec(t.Context(),
			"INSERT INTO ns_migration_basic (key, value, version) VALUES ($1, $2, 0)", []byte("existing"), []byte("value"))
		require.NoError(t, err)
		_, err = env.DB.initFromSnapshot(t.Context(), writeSnapshotBundle(t, migrationNamespace, "value-a"))
		require.ErrorContains(t, err, "is not empty")
	})

	t.Run("when a transaction ID already exists, it rejects the import", func(t *testing.T) {
		env := newSnapshotBootstrapEnv(t, true)
		_, err := env.DB.pool.Exec(t.Context(),
			"INSERT INTO tx_status (tx_id, status, height) VALUES ($1, $2, $3)",
			[]byte("tx-a"), int(committerpb.Status_COMMITTED), make([]byte, 16))
		require.NoError(t, err)
		_, err = env.DB.initFromSnapshot(t.Context(), writeSnapshotBundle(t, migrationNamespace, "value-a"))
		require.ErrorContains(t, err, "transaction IDs already exist")
	})

	t.Run("when target block zero is not committed, it rejects the import", func(t *testing.T) {
		env := NewDatabaseTestEnv(t)
		require.NoError(t, statedb.SetupSystemTablesAndNamespaces(t.Context(), env.DB.config))
		_, err := env.DB.pool.Exec(t.Context(), "INSERT INTO ns__config (key, value, version) VALUES ($1, $2, 0)", []byte("config"), []byte("block-0"))
		require.NoError(t, err)
		_, err = env.DB.initFromSnapshot(t.Context(), writeSnapshotBundle(t, migrationNamespace, "value-a"))
		require.ErrorContains(t, err, "has not committed block 0")
	})

	t.Run("when the target policy has no supported rule, it rejects the import", func(t *testing.T) {
		env := newSnapshotBootstrapEnv(t, true)
		unsupported, err := proto.Marshal(&applicationpb.NamespacePolicy{})
		require.NoError(t, err)
		_, err = env.DB.pool.Exec(t.Context(), "UPDATE ns__meta SET value = $1 WHERE key = $2", unsupported, []byte(migrationNamespace))
		require.NoError(t, err)

		_, err = env.DB.initFromSnapshot(t.Context(), writeSnapshotBundle(t, migrationNamespace, "value-a"))
		require.ErrorContains(t, err, "unsupported policy")
	})

	t.Run("when the target uses a threshold policy, it imports the snapshot", func(t *testing.T) {
		env := newSnapshotBootstrapEnv(t, true)
		_, err := env.DB.pool.Exec(t.Context(), "UPDATE ns__meta SET value = $1 WHERE key = $2", thresholdPolicyBytes(t), []byte(migrationNamespace))
		require.NoError(t, err)

		_, err = env.DB.initFromSnapshot(t.Context(), writeSnapshotBundle(t, migrationNamespace, "value-a"))
		require.NoError(t, err)
	})
}

func testInitFromSnapshotAtomic(t *testing.T) {
	env := newSnapshotBootstrapEnv(t, true)
	_, err := env.DB.pool.Exec(t.Context(), `
ALTER TABLE migration_record
ADD CONSTRAINT fail_verified_insert CHECK (status <> 'VERIFIED')`)
	require.NoError(t, err)

	_, err = env.DB.initFromSnapshot(t.Context(), writeSnapshotBundle(t, migrationNamespace, "value-a"))
	require.ErrorContains(t, err, "failed to write migration record")
	assert.Zero(t, env.getRowCount(t, "SELECT count(*) FROM ns_migration_basic"))
	assert.Zero(t, env.getRowCount(t, "SELECT count(*) FROM migrated_tx_ids"))
	assert.Zero(t, env.getRowCount(t, "SELECT count(*) FROM migration_record"))

	anchor, err := env.DB.getNextBlockNumberToCommit(t.Context())
	require.NoError(t, err)
	assert.Equal(t, uint64(2), anchor.Number)
}

func TestActivateSnapshot(t *testing.T) {
	t.Run("when a verified snapshot is activated, it changes only the migration status", testActivateSnapshot)
}

func testActivateSnapshot(t *testing.T) {
	env := newSnapshotBootstrapEnv(t, true)
	file := writeSnapshotBundle(t, migrationNamespace, "value-a")

	_, err := env.DB.activateSnapshot(t.Context(), file)
	require.ErrorContains(t, err, "has not been imported and verified")
	_, err = env.DB.initFromSnapshot(t.Context(), file)
	require.NoError(t, err)

	_, err = env.DB.pool.Exec(t.Context(), "UPDATE ns_migration_basic SET value = $1 WHERE key = $2", []byte("tampered"), []byte("asset-a"))
	require.NoError(t, err)
	_, err = env.DB.activateSnapshot(t.Context(), file)
	require.ErrorContains(t, err, "target public state does not match")
	assert.Equal(t, 1, env.getRowCount(t, "SELECT count(*) FROM migration_record WHERE status = 'VERIFIED'"))
	_, err = env.DB.pool.Exec(t.Context(), "UPDATE ns_migration_basic SET value = $1 WHERE key = $2", []byte("value-a"), []byte("asset-a"))
	require.NoError(t, err)

	activated, err := env.DB.activateSnapshot(t.Context(), file)
	require.NoError(t, err)
	assert.False(t, activated.AlreadyActive)
	assert.Equal(t, uint64(1), activated.TargetAnchor)
	assert.Equal(t, 1, env.getRowCount(t, "SELECT count(*) FROM migration_record WHERE status = 'ACTIVE'"))

	again, err := env.DB.activateSnapshot(t.Context(), file)
	require.NoError(t, err)
	assert.True(t, again.AlreadyActive)

	_, err = env.DB.initFromSnapshot(t.Context(), file)
	require.ErrorContains(t, err, "ACTIVE; snapshot import is disabled")
	assert.Equal(t, 2, env.getRowCount(t, "SELECT count(*) FROM ns_migration_basic"))
	assert.Equal(t, 2, env.getRowCount(t, "SELECT count(*) FROM migrated_tx_ids"))

	_, err = env.DB.activateSnapshot(t.Context(), writeSnapshotBundle(t, migrationNamespace, "different-value"))
	require.ErrorContains(t, err, "different genesis-data file")
}

func TestVerifySnapshot(t *testing.T) {
	env := newSnapshotBootstrapEnv(t, true)
	file := writeSnapshotBundle(t, migrationNamespace, "value-a")

	t.Run("when the snapshot has not been imported, it rejects verification", func(t *testing.T) {
		_, err := env.DB.verifySnapshot(t.Context(), file)
		require.ErrorContains(t, err, "has not been imported")
	})

	_, err := env.DB.initFromSnapshot(t.Context(), file)
	require.NoError(t, err)

	t.Run("when target state matches, it reports verified integrity", func(t *testing.T) {
		verified, err := env.DB.verifySnapshot(t.Context(), file)
		require.NoError(t, err)
		assert.Equal(t, file.MigrationID, verified.MigrationID)
		assert.Equal(t, "VERIFIED", verified.MigrationStatus)
		assert.Equal(t, uint64(1), verified.TargetAnchor)
		assert.Len(t, verified.TargetConfigSHA256, 64)
		assert.Len(t, verified.NamespaceMapSHA256, 64)
		assert.Len(t, verified.TargetPolicySHA256, 64)
		assert.Equal(t, uint64(2), verified.PublicStateCount)
		assert.Len(t, verified.PublicStateSHA256, 64)
		assert.Equal(t, uint64(2), verified.TransactionIDCount)
		assert.Len(t, verified.TransactionIDSHA256, 64)
	})

	t.Run("when public state is changed, it rejects verification", func(t *testing.T) {
		_, err := env.DB.pool.Exec(t.Context(), "UPDATE ns_migration_basic SET value = $1 WHERE key = $2", []byte("tampered"), []byte("asset-a"))
		require.NoError(t, err)
		_, err = env.DB.verifySnapshot(t.Context(), file)
		require.ErrorContains(t, err, "target public state does not match")
		_, err = env.DB.pool.Exec(t.Context(), "UPDATE ns_migration_basic SET value = $1 WHERE key = $2", []byte("value-a"), []byte("asset-a"))
		require.NoError(t, err)
	})

	t.Run("when the transaction ID baseline changes, it rejects verification", func(t *testing.T) {
		_, err := env.DB.pool.Exec(t.Context(), "INSERT INTO migrated_tx_ids (tx_id) VALUES ($1)", []byte("tx-extra"))
		require.NoError(t, err)
		_, err = env.DB.verifySnapshot(t.Context(), file)
		require.ErrorContains(t, err, "target transaction IDs do not match")
		_, err = env.DB.pool.Exec(t.Context(), "DELETE FROM migrated_tx_ids WHERE tx_id = $1", []byte("tx-extra"))
		require.NoError(t, err)
	})

	t.Run("when the namespace policy changes, it rejects verification", func(t *testing.T) {
		_, err := env.DB.pool.Exec(t.Context(), "UPDATE ns__meta SET value = $1 WHERE key = $2", mspPolicyBytes(t, "OR('Org1MSP.admin')"), []byte(migrationNamespace))
		require.NoError(t, err)
		_, err = env.DB.verifySnapshot(t.Context(), file)
		require.ErrorContains(t, err, "target bindings")
		_, err = env.DB.pool.Exec(t.Context(), "UPDATE ns__meta SET value = $1 WHERE key = $2", mspPolicyBytes(t, "OR('Org1MSP.member')"), []byte(migrationNamespace))
		require.NoError(t, err)
	})

	t.Run("when the target anchor advances, it rejects verification", func(t *testing.T) {
		require.NoError(t, env.DB.setLastCommittedBlockNumber(t.Context(), &servicepb.BlockRef{Number: 2}))
		_, err := env.DB.verifySnapshot(t.Context(), file)
		require.ErrorContains(t, err, "advanced beyond")
		require.NoError(t, env.DB.setLastCommittedBlockNumber(t.Context(), &servicepb.BlockRef{Number: 1}))
	})

	t.Run("when the migration is active, it still verifies integrity", func(t *testing.T) {
		_, err := env.DB.activateSnapshot(t.Context(), file)
		require.NoError(t, err)
		active, err := env.DB.verifySnapshot(t.Context(), file)
		require.NoError(t, err)
		assert.Equal(t, "ACTIVE", active.MigrationStatus)
	})
}

func testInitFromSnapshotMultipleNamespaces(t *testing.T) {
	env := newSnapshotBootstrapEnv(t, false)
	env.populateData(t, []string{"migration_assets", "migration_payments"}, nil, nil, nil)
	for _, namespace := range []string{"migration_assets", "migration_payments"} {
		_, err := env.DB.pool.Exec(t.Context(), "UPDATE ns__meta SET value = $1 WHERE key = $2", mspPolicyBytes(t, "OR('Org1MSP.member')"), []byte(namespace))
		require.NoError(t, err)
	}
	file := writeMultipleNamespaceBundle(t)

	result, err := env.DB.initFromSnapshot(t.Context(), file)
	require.NoError(t, err)
	assert.Equal(t, uint64(2), result.PublicStateCount)
	assert.Equal(t, 1, env.getRowCount(t, "SELECT count(*) FROM ns_migration_assets"))
	assert.Equal(t, 1, env.getRowCount(t, "SELECT count(*) FROM ns_migration_payments"))

	verified, err := env.DB.verifySnapshot(t.Context(), file)
	require.NoError(t, err)
	assert.Equal(t, uint64(2), verified.PublicStateCount)
	assert.Len(t, verified.PublicStateSHA256, 64)
}

func newSnapshotBootstrapEnv(t *testing.T, createNamespace bool) *DatabaseTestEnv {
	t.Helper()
	env := NewDatabaseTestEnv(t)
	require.NoError(t, statedb.SetupSystemTablesAndNamespaces(t.Context(), env.DB.config))
	_, err := env.DB.pool.Exec(t.Context(),
		"INSERT INTO ns__config (key, value, version) VALUES ($1, $2, 0)", []byte("config"), []byte("block-0"))
	require.NoError(t, err)
	if createNamespace {
		env.populateData(t, []string{migrationNamespace}, nil, nil, nil)
		_, err = env.DB.pool.Exec(t.Context(), "UPDATE ns__meta SET value = $1 WHERE key = $2", mspPolicyBytes(t, "OR('Org1MSP.member')"), []byte(migrationNamespace))
		require.NoError(t, err)
	}
	require.NoError(t, env.DB.setLastCommittedBlockNumber(t.Context(), &servicepb.BlockRef{Number: 1}))
	return env
}

func mspPolicyBytes(t *testing.T, expression string) []byte {
	t.Helper()
	envelope, err := policydsl.FromString(expression)
	require.NoError(t, err)
	envelopeBytes, err := proto.Marshal(envelope)
	require.NoError(t, err)
	policyBytes, err := proto.Marshal(&applicationpb.NamespacePolicy{
		Rule: &applicationpb.NamespacePolicy_MspRule{MspRule: envelopeBytes},
	})
	require.NoError(t, err)
	return policyBytes
}

func thresholdPolicyBytes(t *testing.T) []byte {
	t.Helper()
	_, verificationKey := testsig.NewKeyPair(signature.Ecdsa)
	policyBytes, err := proto.Marshal(verifierpolicy.MakeECDSAThresholdRuleNsPolicy(verificationKey))
	require.NoError(t, err)
	return policyBytes
}

func writeSnapshotBundle(t *testing.T, targetNamespace, firstValue string) *genesisdata.File {
	t.Helper()
	input := genesisdata.Input{
		ExporterVersion: "integration-test",
		Source: genesisdata.Source{
			FabricVersion: "2.5.16", Channel: "migration", LastBlockNumber: 3,
			LastBlockHash: strings.Repeat("a", 64), PreviousBlockHash: strings.Repeat("b", 64),
			SnapshotHash: strings.Repeat("c", 64), StateDBType: "SimpleKeyValueDB",
		},
		NamespaceMappings: []genesisdata.NamespaceMapping{{Source: "basic", Target: targetNamespace}},
		PublicState: []*genesisdata.StateRecord{
			{SourceNamespace: "basic", TargetNamespace: targetNamespace, Key: []byte("asset-a"), Value: []byte(firstValue)},
			{SourceNamespace: "basic", TargetNamespace: targetNamespace, Key: []byte("asset-b"), Value: []byte("value-b")},
		},
		TransactionIDs: []*genesisdata.TransactionIDRecord{{TransactionId: "tx-a"}, {TransactionId: "tx-b"}},
	}
	path := filepath.Join(t.TempDir(), "snapshot.fxgenesis")
	file, err := genesisdata.Write(path, input)
	require.NoError(t, err)
	return file
}

func writeMultipleNamespaceBundle(t *testing.T) *genesisdata.File {
	t.Helper()
	input := genesisdata.Input{
		ExporterVersion: "integration-test",
		Source: genesisdata.Source{
			FabricVersion: "3.1.5", Channel: "channel-a", LastBlockNumber: 3,
			LastBlockHash: strings.Repeat("a", 64), PreviousBlockHash: strings.Repeat("b", 64),
			SnapshotHash: strings.Repeat("c", 64), StateDBType: "SimpleKeyValueDB",
		},
		NamespaceMappings: []genesisdata.NamespaceMapping{
			{Source: "assets", Target: "migration_assets"},
			{Source: "payments", Target: "migration_payments"},
		},
		PublicState: []*genesisdata.StateRecord{
			{SourceNamespace: "assets", TargetNamespace: "migration_assets", Key: []byte("asset-a"), Value: []byte("value-a")},
			{SourceNamespace: "payments", TargetNamespace: "migration_payments", Key: []byte("payment-a"), Value: []byte("value-b")},
		},
		TransactionIDs: []*genesisdata.TransactionIDRecord{{TransactionId: "tx-a"}, {TransactionId: "tx-b"}},
	}
	path := filepath.Join(t.TempDir(), "multiple-namespaces.fxgenesis")
	file, err := genesisdata.Write(path, input)
	require.NoError(t, err)
	return file
}

func TestAppendDigestBytes(t *testing.T) {
	t.Run("when byte partitions differ, it produces distinct framed digests", func(t *testing.T) {
		left := bytes.NewBuffer(nil)
		appendDigestBytes(left, []byte("ab"))
		appendDigestBytes(left, []byte("c"))
		right := bytes.NewBuffer(nil)
		appendDigestBytes(right, []byte("a"))
		appendDigestBytes(right, []byte("bc"))
		assert.NotEqual(t, left.Bytes(), right.Bytes())
	})
}
