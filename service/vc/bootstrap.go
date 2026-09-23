/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package vc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"sort"

	"github.com/cockroachdb/errors"
	cb "github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/yugabyte/pgx/v5"
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-committer/service/verifier/policy"
	"github.com/hyperledger/fabric-x-committer/utils/signature"
	"github.com/hyperledger/fabric-x-committer/utils/statedb"
	"github.com/syndbg/fabric-x-migrate-poc/pkg/genesisdata"
)

const bootstrapLockID int64 = 0x464142584d494752 // FABXMIGR

// SnapshotInitResult is the verified target state created by InitFromSnapshot.
type SnapshotInitResult struct {
	MigrationID       string
	SourceChannel     string
	SourceBlockNumber uint64
	TargetAnchor      uint64
	PublicStateCount  uint64
	TransactionIDs    uint64
	AlreadyImported   bool
}

// SnapshotActivationResult is the verified migration record activated by
// ActivateSnapshot.
type SnapshotActivationResult struct {
	MigrationID   string
	TargetAnchor  uint64
	AlreadyActive bool
}

// SnapshotVerificationResult describes a successful comparison between a
// genesis-data file, the migration record, and target state.
type SnapshotVerificationResult struct {
	MigrationID         string
	MigrationStatus     string
	SourceChannel       string
	SourceBlockNumber   uint64
	TargetAnchor        uint64
	TargetConfigSHA256  string
	NamespaceMapSHA256  string
	TargetPolicySHA256  string
	PublicStateCount    uint64
	PublicStateSHA256   string
	TransactionIDCount  uint64
	TransactionIDSHA256 string
}

type migrationStatus string

const (
	migrationAbsent   migrationStatus = ""
	migrationVerified migrationStatus = "VERIFIED"
	migrationActive   migrationStatus = "ACTIVE"
)

type bootstrapDigests struct {
	config       []byte
	namespaceMap []byte
	policy       []byte
	state        []byte
	txIDs        []byte
}

// InitFromSnapshot verifies a genesis-data file and imports it into an idle
// Validator-Committer database without advancing the Fabric-X block height.
func InitFromSnapshot(ctx context.Context, config *Config, path string) (*SnapshotInitResult, error) {
	file, db, err := openSnapshotDatabase(ctx, config, path)
	if err != nil {
		return nil, err
	}
	defer db.close()
	if err := statedb.SetupSystemTablesAndNamespaces(ctx, config.Database); err != nil {
		return nil, err
	}
	return db.initFromSnapshot(ctx, file)
}

func (db *database) initFromSnapshot(ctx context.Context, file *genesisdata.File) (*SnapshotInitResult, error) {
	tx, err := db.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return nil, errors.Wrap(err, "failed to begin snapshot initialization")
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", bootstrapLockID); err != nil {
		return nil, errors.Wrap(err, "failed to acquire snapshot initialization lock")
	}

	anchor, err := readTargetAnchor(ctx, tx)
	if err != nil {
		return nil, err
	}
	digests, err := inspectTarget(ctx, tx, file)
	if err != nil {
		return nil, err
	}
	result := snapshotInitResult(file, anchor)

	status, err := verifyExistingMigration(ctx, tx, file, anchor, digests)
	if err != nil {
		return nil, err
	}
	switch status {
	case migrationActive:
		return nil, errors.New("target migration is ACTIVE; snapshot import is disabled")
	case migrationVerified:
		result.AlreadyImported = true
		return result, nil
	}
	if err := requireEmptyTarget(ctx, tx, file); err != nil {
		return nil, err
	}
	if err := insertSnapshotState(ctx, tx, file); err != nil {
		return nil, err
	}
	if err := verifyLiveTarget(ctx, tx, file, digests.state, digests.txIDs); err != nil {
		return nil, err
	}
	if err := insertMigrationRecord(ctx, tx, file, anchor, digests); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, errors.Wrap(err, "failed to commit snapshot initialization")
	}
	return result, nil
}

// ActivateSnapshot verifies an existing snapshot import and changes only its
// migration record from VERIFIED to ACTIVE.
func ActivateSnapshot(ctx context.Context, config *Config, path string) (*SnapshotActivationResult, error) {
	file, db, err := openSnapshotDatabase(ctx, config, path)
	if err != nil {
		return nil, err
	}
	defer db.close()
	return db.activateSnapshot(ctx, file)
}

// VerifySnapshot independently compares a genesis-data file with the
// target database and its migration record without changing either.
func VerifySnapshot(ctx context.Context, config *Config, path string) (*SnapshotVerificationResult, error) {
	file, db, err := openSnapshotDatabase(ctx, config, path)
	if err != nil {
		return nil, err
	}
	defer db.close()
	return db.verifySnapshot(ctx, file)
}

func openSnapshotDatabase(ctx context.Context, config *Config, path string) (*genesisdata.File, *database, error) {
	if config == nil || config.Database == nil {
		return nil, nil, errors.New("validator-committer database configuration is required")
	}
	file, err := genesisdata.Read(path)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to verify genesis-data file")
	}
	db, err := newDatabase(ctx, config.Database, newVCServiceMetrics(), config.ResourceLimits)
	if err != nil {
		return nil, nil, err
	}
	return file, db, nil
}

func (db *database) verifySnapshot(ctx context.Context, file *genesisdata.File) (*SnapshotVerificationResult, error) {
	tx, err := db.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, errors.Wrap(err, "failed to begin snapshot verification")
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", bootstrapLockID); err != nil {
		return nil, errors.Wrap(err, "failed to acquire snapshot verification lock")
	}
	anchor, err := readTargetAnchor(ctx, tx)
	if err != nil {
		return nil, err
	}
	digests, err := inspectTarget(ctx, tx, file)
	if err != nil {
		return nil, err
	}
	status, err := verifyExistingMigration(ctx, tx, file, anchor, digests)
	if err != nil {
		return nil, err
	}
	if status == migrationAbsent {
		return nil, errors.New("snapshot has not been imported")
	}
	return &SnapshotVerificationResult{
		MigrationID: file.MigrationID, MigrationStatus: string(status),
		SourceChannel: file.Manifest.Source.Channel, SourceBlockNumber: file.Manifest.Source.LastBlockNumber,
		TargetAnchor: anchor, TargetConfigSHA256: hex.EncodeToString(digests.config),
		NamespaceMapSHA256: hex.EncodeToString(digests.namespaceMap), TargetPolicySHA256: hex.EncodeToString(digests.policy),
		PublicStateCount: uint64(len(file.PublicState)), PublicStateSHA256: hex.EncodeToString(digests.state),
		TransactionIDCount: uint64(len(file.TransactionIDs)), TransactionIDSHA256: hex.EncodeToString(digests.txIDs),
	}, nil
}

func (db *database) activateSnapshot(ctx context.Context, file *genesisdata.File) (*SnapshotActivationResult, error) {
	tx, err := db.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return nil, errors.Wrap(err, "failed to begin snapshot activation")
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", bootstrapLockID); err != nil {
		return nil, errors.Wrap(err, "failed to acquire snapshot activation lock")
	}
	anchor, err := readTargetAnchor(ctx, tx)
	if err != nil {
		return nil, err
	}
	digests, err := inspectTarget(ctx, tx, file)
	if err != nil {
		return nil, err
	}
	status, err := verifyExistingMigration(ctx, tx, file, anchor, digests)
	if err != nil {
		return nil, err
	}
	result := &SnapshotActivationResult{MigrationID: file.MigrationID, TargetAnchor: anchor}
	switch status {
	case migrationAbsent:
		return nil, errors.New("snapshot has not been imported and verified")
	case migrationActive:
		result.AlreadyActive = true
		return result, nil
	}
	command, err := tx.Exec(ctx, "UPDATE migration_record SET status = 'ACTIVE' WHERE singleton_id = 1 AND status = 'VERIFIED'")
	if err != nil {
		return nil, errors.Wrap(err, "failed to activate migration record")
	}
	if command.RowsAffected() != 1 {
		return nil, errors.New("migration record was not VERIFIED during activation")
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, errors.Wrap(err, "failed to commit snapshot activation")
	}
	return result, nil
}

func snapshotInitResult(file *genesisdata.File, anchor uint64) *SnapshotInitResult {
	return &SnapshotInitResult{
		MigrationID:       file.MigrationID,
		SourceChannel:     file.Manifest.Source.Channel,
		SourceBlockNumber: file.Manifest.Source.LastBlockNumber,
		TargetAnchor:      anchor,
		PublicStateCount:  uint64(len(file.PublicState)),
		TransactionIDs:    uint64(len(file.TransactionIDs)),
	}
}

func readTargetAnchor(ctx context.Context, tx pgx.Tx) (uint64, error) {
	var value []byte
	if err := tx.QueryRow(ctx, getMetadataPrepSQLStmt, lastCommittedBlockNumberKey).Scan(&value); err != nil {
		return 0, errors.Wrap(err, "failed to read target anchor")
	}
	if len(value) != 8 {
		return 0, errors.New("target has not committed block 0")
	}
	return binary.BigEndian.Uint64(value), nil
}

func inspectTarget(ctx context.Context, tx pgx.Tx, file *genesisdata.File) (*bootstrapDigests, error) {
	configHash, err := hashRows(ctx, tx, "SELECT key, value, version FROM ns__config ORDER BY key")
	if err != nil {
		return nil, errors.Wrap(err, "failed to hash target configuration")
	}
	if configHash.count == 0 {
		return nil, errors.New("target configuration namespace is empty")
	}

	mappings := append([]genesisdata.NamespaceMapping(nil), file.Manifest.NamespaceMappings...)
	sort.Slice(mappings, func(i, j int) bool { return mappings[i].Source < mappings[j].Source })
	mapHash := sha256.New()
	policyHash := sha256.New()
	for _, mapping := range mappings {
		if err := validateTargetNamespace(mapping.Target); err != nil {
			return nil, err
		}
		appendDigestBytes(mapHash, []byte(mapping.Source))
		appendDigestBytes(mapHash, []byte(mapping.Target))
		var policyValue []byte
		var version int64
		if err := tx.QueryRow(ctx, "SELECT value, version FROM ns__meta WHERE key = $1", []byte(mapping.Target)).Scan(&policyValue, &version); err != nil {
			return nil, errors.Wrapf(err, "target namespace %q is not installed", mapping.Target)
		}
		if err := validateTargetPolicy(policyValue); err != nil {
			return nil, errors.Wrapf(err, "target namespace %q has an unsupported policy", mapping.Target)
		}
		appendDigestBytes(policyHash, []byte(mapping.Target))
		appendDigestBytes(policyHash, policyValue)
		appendDigestUint64(policyHash, uint64(version))
	}

	expectedState := hashExpectedState(file)
	expectedTxIDs := hashExpectedTxIDs(file)
	return &bootstrapDigests{
		config: configHash.hash, namespaceMap: mapHash.Sum(nil), policy: policyHash.Sum(nil),
		state: expectedState.hash, txIDs: expectedTxIDs.hash,
	}, nil
}

func validateTargetPolicy(policyValue []byte) error {
	namespacePolicy, err := policy.UnmarshalNamespacePolicy(policyValue)
	if err != nil {
		return err
	}
	switch rule := namespacePolicy.Rule.(type) {
	case *applicationpb.NamespacePolicy_ThresholdRule:
		if rule.ThresholdRule == nil {
			return errors.New("threshold policy is empty")
		}
		_, err := signature.NewNsVerifierFromKey(rule.ThresholdRule.Scheme, rule.ThresholdRule.PublicKey)
		return err
	case *applicationpb.NamespacePolicy_MspRule:
		envelope := &cb.SignaturePolicyEnvelope{}
		if err := proto.Unmarshal(rule.MspRule, envelope); err != nil {
			return errors.Wrap(err, "invalid MSP policy")
		}
		if envelope.Rule == nil || len(envelope.Identities) == 0 {
			return errors.New("MSP policy is empty")
		}
		return nil
	default:
		return errors.New("policy rule is not supported")
	}
}

func validateTargetNamespace(namespace string) error {
	if namespace == committerpb.MetaNamespaceID || namespace == committerpb.ConfigNamespaceID {
		return errors.Newf("system namespace %q cannot receive imported state", namespace)
	}
	if err := policy.ValidateNamespaceID(namespace); err != nil {
		return errors.Wrapf(err, "invalid target namespace %q", namespace)
	}
	return nil
}

func verifyExistingMigration(
	ctx context.Context,
	tx pgx.Tx,
	file *genesisdata.File,
	anchor uint64,
	digests *bootstrapDigests,
) (migrationStatus, error) {
	var migrationID, snapshotHash, configHash, namespaceMapHash, policyHash, stateHash, txIDHash []byte
	var sourceBlock, storedAnchor, stateCount, txIDCount int64
	var sourceChannel string
	var status string
	err := tx.QueryRow(ctx, `
SELECT migration_id, source_channel, source_block_number, source_snapshot_hash,
       target_anchor, target_config_hash, namespace_map_hash, target_policy_hash,
       public_state_count, public_state_hash, transaction_id_count,
       transaction_id_hash, status
FROM migration_record WHERE singleton_id = 1`).Scan(
		&migrationID, &sourceChannel, &sourceBlock, &snapshotHash, &storedAnchor,
		&configHash, &namespaceMapHash, &policyHash, &stateCount, &stateHash,
		&txIDCount, &txIDHash, &status,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return migrationAbsent, nil
	}
	if err != nil {
		return migrationAbsent, errors.Wrap(err, "failed to read migration record")
	}
	expectedID, err := decodeHash("migration ID", file.MigrationID)
	if err != nil {
		return migrationAbsent, err
	}
	if !bytes.Equal(migrationID, expectedID) {
		return migrationAbsent, errors.New("target was initialized from a different genesis-data file")
	}
	expectedSnapshotHash, err := decodeHash("source snapshot hash", file.Manifest.Source.SnapshotHash)
	if err != nil {
		return migrationAbsent, err
	}
	if storedAnchor < 0 || uint64(storedAnchor) != anchor {
		return migrationAbsent, errors.New("target advanced beyond the recorded migration anchor")
	}
	if sourceBlock < 0 || stateCount < 0 || txIDCount < 0 ||
		sourceChannel != file.Manifest.Source.Channel || uint64(sourceBlock) != file.Manifest.Source.LastBlockNumber ||
		!bytes.Equal(snapshotHash, expectedSnapshotHash) ||
		!bytes.Equal(configHash, digests.config) || !bytes.Equal(namespaceMapHash, digests.namespaceMap) ||
		!bytes.Equal(policyHash, digests.policy) || uint64(stateCount) != uint64(len(file.PublicState)) ||
		uint64(txIDCount) != uint64(len(file.TransactionIDs)) ||
		!bytes.Equal(stateHash, digests.state) || !bytes.Equal(txIDHash, digests.txIDs) {
		return migrationAbsent, errors.New("migration record no longer matches its source or target bindings")
	}
	if migrationStatus(status) != migrationVerified && migrationStatus(status) != migrationActive {
		return migrationAbsent, errors.Newf("unsupported migration status %q", status)
	}
	if err := verifyLiveTarget(ctx, tx, file, digests.state, digests.txIDs); err != nil {
		return migrationAbsent, err
	}
	return migrationStatus(status), nil
}

func requireEmptyTarget(ctx context.Context, tx pgx.Tx, file *genesisdata.File) error {
	for _, mapping := range file.Manifest.NamespaceMappings {
		var exists bool
		query := "SELECT EXISTS (SELECT 1 FROM " + statedb.TableName(mapping.Target) + " LIMIT 1)"
		if err := tx.QueryRow(ctx, query).Scan(&exists); err != nil {
			return errors.Wrapf(err, "failed to inspect target namespace %q", mapping.Target)
		}
		if exists {
			return errors.Newf("target namespace %q is not empty", mapping.Target)
		}
	}
	if len(file.TransactionIDs) == 0 {
		return nil
	}
	ids := transactionIDBytes(file)
	var count int64
	if err := tx.QueryRow(ctx, `
SELECT (SELECT count(*) FROM tx_status WHERE tx_id = ANY($1)) +
       (SELECT count(*) FROM migrated_tx_ids WHERE tx_id = ANY($1))`, ids).Scan(&count); err != nil {
		return errors.Wrap(err, "failed to inspect target transaction IDs")
	}
	if count != 0 {
		return errors.New("one or more source transaction IDs already exist in the target")
	}
	return nil
}

func insertSnapshotState(ctx context.Context, tx pgx.Tx, file *genesisdata.File) error {
	type records struct {
		keys   [][]byte
		values [][]byte
	}
	byNamespace := map[string]*records{}
	for _, record := range file.PublicState {
		group := byNamespace[record.TargetNamespace]
		if group == nil {
			group = &records{}
			byNamespace[record.TargetNamespace] = group
		}
		group.keys = append(group.keys, record.Key)
		group.values = append(group.values, record.Value)
	}
	for namespace, group := range byNamespace {
		versions := make([]int64, len(group.keys))
		query := "INSERT INTO " + statedb.TableName(namespace) + ` (key, value, version)
SELECT key, value, version
FROM unnest($1::bytea[], $2::bytea[], $3::bigint[]) AS rows(key, value, version)`
		if _, err := tx.Exec(ctx, query, group.keys, group.values, versions); err != nil {
			return errors.Wrapf(err, "failed to import namespace %q", namespace)
		}
	}
	if len(file.TransactionIDs) > 0 {
		if _, err := tx.Exec(ctx,
			"INSERT INTO migrated_tx_ids (tx_id) SELECT unnest($1::bytea[])", transactionIDBytes(file)); err != nil {
			return errors.Wrap(err, "failed to import transaction IDs")
		}
	}
	return nil
}

func verifyLiveTarget(ctx context.Context, tx pgx.Tx, file *genesisdata.File, stateHash, txIDHash []byte) error {
	liveState := sha256.New()
	var stateCount uint64
	mappings := append([]genesisdata.NamespaceMapping(nil), file.Manifest.NamespaceMappings...)
	sort.Slice(mappings, func(i, j int) bool { return mappings[i].Target < mappings[j].Target })
	for _, mapping := range mappings {
		rows, err := tx.Query(ctx, "SELECT key, value, version FROM "+statedb.TableName(mapping.Target)+" ORDER BY key")
		if err != nil {
			return errors.Wrapf(err, "failed to verify namespace %q", mapping.Target)
		}
		for rows.Next() {
			var key, value []byte
			var version int64
			if err := rows.Scan(&key, &value, &version); err != nil {
				rows.Close()
				return errors.Wrap(err, "failed to read imported state")
			}
			if version < 0 {
				rows.Close()
				return errors.New("imported state has a negative target version")
			}
			appendStateDigest(liveState, mapping.Target, key, value, uint64(version))
			stateCount++
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return errors.Wrap(err, "failed to scan imported state")
		}
		rows.Close()
	}
	if stateCount != uint64(len(file.PublicState)) || !bytes.Equal(liveState.Sum(nil), stateHash) {
		return errors.New("target public state does not match genesis-data file")
	}

	liveTxIDs, err := tx.Query(ctx, "SELECT tx_id FROM migrated_tx_ids ORDER BY tx_id")
	if err != nil {
		return errors.Wrap(err, "failed to verify migrated transaction IDs")
	}
	txHasher := sha256.New()
	var txIDCount uint64
	for liveTxIDs.Next() {
		var id []byte
		if err := liveTxIDs.Scan(&id); err != nil {
			liveTxIDs.Close()
			return errors.Wrap(err, "failed to read migrated transaction ID")
		}
		appendDigestBytes(txHasher, id)
		txIDCount++
	}
	if err := liveTxIDs.Err(); err != nil {
		liveTxIDs.Close()
		return errors.Wrap(err, "failed to scan migrated transaction IDs")
	}
	liveTxIDs.Close()
	if txIDCount != uint64(len(file.TransactionIDs)) || !bytes.Equal(txHasher.Sum(nil), txIDHash) {
		return errors.New("target transaction IDs do not match genesis-data file")
	}
	return nil
}

func insertMigrationRecord(
	ctx context.Context,
	tx pgx.Tx,
	file *genesisdata.File,
	anchor uint64,
	digests *bootstrapDigests,
) error {
	if file.Manifest.Source.LastBlockNumber > math.MaxInt64 || anchor > math.MaxInt64 {
		return errors.New("source or target block number exceeds database range")
	}
	migrationID, err := decodeHash("migration ID", file.MigrationID)
	if err != nil {
		return err
	}
	snapshotHash, err := decodeHash("source snapshot hash", file.Manifest.Source.SnapshotHash)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
INSERT INTO migration_record (
    singleton_id, migration_id, source_channel, source_block_number,
    source_snapshot_hash, target_anchor, target_config_hash, namespace_map_hash,
    target_policy_hash, public_state_count, public_state_hash,
    transaction_id_count, transaction_id_hash, status
) VALUES (1, $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, 'VERIFIED')`,
		migrationID, file.Manifest.Source.Channel, int64(file.Manifest.Source.LastBlockNumber),
		snapshotHash, int64(anchor), digests.config, digests.namespaceMap, digests.policy,
		int64(len(file.PublicState)), digests.state, int64(len(file.TransactionIDs)), digests.txIDs)
	return errors.Wrap(err, "failed to write migration record")
}

type rowHash struct {
	count uint64
	hash  []byte
}

func hashRows(ctx context.Context, tx pgx.Tx, query string) (*rowHash, error) {
	rows, err := tx.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	hasher := sha256.New()
	var count uint64
	for rows.Next() {
		var key, value []byte
		var version int64
		if err := rows.Scan(&key, &value, &version); err != nil {
			return nil, err
		}
		appendDigestBytes(hasher, key)
		appendDigestBytes(hasher, value)
		appendDigestUint64(hasher, uint64(version))
		count++
	}
	return &rowHash{count: count, hash: hasher.Sum(nil)}, rows.Err()
}

func hashExpectedState(file *genesisdata.File) *rowHash {
	hasher := sha256.New()
	for _, record := range file.PublicState {
		appendStateDigest(hasher, record.TargetNamespace, record.Key, record.Value, 0)
	}
	return &rowHash{count: uint64(len(file.PublicState)), hash: hasher.Sum(nil)}
}

func hashExpectedTxIDs(file *genesisdata.File) *rowHash {
	hasher := sha256.New()
	for _, record := range file.TransactionIDs {
		appendDigestBytes(hasher, []byte(record.TransactionId))
	}
	return &rowHash{count: uint64(len(file.TransactionIDs)), hash: hasher.Sum(nil)}
}

func appendStateDigest(hasher interface{ Write([]byte) (int, error) }, namespace string, key, value []byte, version uint64) {
	appendDigestBytes(hasher, []byte(namespace))
	appendDigestBytes(hasher, key)
	appendDigestBytes(hasher, value)
	appendDigestUint64(hasher, version)
}

func appendDigestBytes(hasher interface{ Write([]byte) (int, error) }, value []byte) {
	var length [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(length[:], uint64(len(value)))
	_, _ = hasher.Write(length[:n])
	_, _ = hasher.Write(value)
}

func appendDigestUint64(hasher interface{ Write([]byte) (int, error) }, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	_, _ = hasher.Write(encoded[:])
}

func transactionIDBytes(file *genesisdata.File) [][]byte {
	result := make([][]byte, len(file.TransactionIDs))
	for i, record := range file.TransactionIDs {
		result[i] = []byte(record.TransactionId)
	}
	return result
}

func decodeHash(name, value string) ([]byte, error) {
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size {
		return nil, fmt.Errorf("invalid %s", name)
	}
	return decoded, nil
}
