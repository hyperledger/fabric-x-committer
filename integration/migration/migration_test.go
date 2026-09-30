//go:build integration

/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package migration_test

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	ab "github.com/hyperledger/fabric-protos-go-apiv2/orderer"
	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/ordererdial"

	"github.com/syndbg/fabric-x-migrate-poc/pkg/genesisdata"
)

func TestInitFromSnapshot(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Docker and the Fabric-X devnet")
	}

	_, sourceFile, _, _ := runtime.Caller(0)
	committerRepo := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", ".."))
	workspace := filepath.Dir(committerRepo)
	devnet := filepath.Join(workspace, "fabric-x-samples", "devnet")
	migrateRepo := filepath.Join(workspace, "fabric-x-migrate-poc")
	labRepo := filepath.Join(workspace, "fabric-x-migration-lab")

	committerBinary := filepath.Join(t.TempDir(), "committer")
	build := exec.Command("go", "build", "-o", committerBinary, "./cmd/committer")
	build.Dir = committerRepo
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	runCommand(t, build)
	replayBinary := filepath.Join(t.TempDir(), "replayblocks")
	buildReplay := exec.Command("go", "build", "-o", replayBinary, "./integration/migration/replayblocks")
	buildReplay.Dir = committerRepo
	buildReplay.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	runCommand(t, buildReplay)
	freezeBinary := filepath.Join(t.TempDir(), "freezecheck")
	buildFreeze := exec.Command("go", "build", "-o", freezeBinary, "./integration/migration/freezecheck")
	buildFreeze.Dir = committerRepo
	buildFreeze.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	runCommand(t, buildFreeze)

	if containerRunning(t, "orderer.example.com") {
		run(t, "", "docker", "stop", "orderer.example.com")
		t.Cleanup(func() { run(t, "", "docker", "start", "orderer.example.com") })
	}
	t.Cleanup(func() { run(t, devnet, "make", "purge") })

	for _, version := range []string{"2.5.16", "3.1.5"} {
		t.Run("when source is Fabric "+version+", it imports, activates, and updates migrated state", func(t *testing.T) {
			snapshotPath := filepath.Join(labRepo, "artifacts", "snapshots", "fabric-"+version, "block-3")
			exerciseMigration(t, devnet, migrateRepo, committerBinary, freezeBinary, snapshotPath, version, "migration", "migration_basic")
		})
	}

	t.Run("when source is Fabric 3.1.5 with CouchDB, it completes the full migration", func(t *testing.T) {
		snapshotPath := captureFabricSnapshots(t, migrateRepo, "CouchDB", "couchdb")["couchdb"]
		exerciseMigration(t, devnet, migrateRepo, committerBinary, freezeBinary, snapshotPath, fabricSourceVersion, "couchdb", "couchdb_basic")
	})

	t.Run("when two channels map to separate networks, it imports both snapshots", func(t *testing.T) {
		source := filepath.Join(labRepo, "artifacts", "snapshots", "fabric-3.1.5", "block-3")
		migrationIDs := make([]string, 0, 2)
		for _, channel := range []string{"payments", "securities"} {
			t.Run("when source channel is "+channel+", it imports and updates state in a fresh network", func(t *testing.T) {
				snapshotPath := cloneSnapshotWithChannel(t, source, channel)
				migrationIDs = append(migrationIDs, exerciseMigration(t, devnet, migrateRepo, committerBinary, freezeBinary, snapshotPath, "3.1.5", channel, channel+"_basic"))
			})
		}
		require.Len(t, migrationIDs, 2)
		assert.NotEqual(t, migrationIDs[0], migrationIDs[1], "source channels must produce distinct migrations")
	})

	t.Run("when two committer organizations migrate the same network, it activates and commits on both", func(t *testing.T) {
		run(t, devnet, "make", "purge")
		t.Cleanup(func() { run(t, devnet, "make", "purge") })
		run(t, devnet, "make", "init")
		run(t, devnet, "make", "start-both")
		run(t, devnet, "make", "init-namespace-org1", "NS=multi_org_basic")

		snapshotPath := filepath.Join(labRepo, "artifacts", "snapshots", "fabric-3.1.5", "block-3")
		bundlePath, bundle := exportBundle(t, migrateRepo, snapshotPath, "3.1.5", "basic", "multi_org_basic")
		anchorHex, anchor := targetAnchor(t, devnet, "committer-db", "multi_org_basic")
		org2AnchorHex, org2Anchor := targetAnchor(t, devnet, "committer-org2-db", "multi_org_basic")
		assert.Equal(t, anchorHex, org2AnchorHex)
		assert.Equal(t, anchor, org2Anchor)

		freezeBothAtAnchor(t, devnet, freezeBinary, anchor)
		org1Config := writeBootstrapConfig(t, "committer-db:5432")
		org2Config := writeBootstrapConfig(t, "committer-org2-db:5432")
		assert.Contains(t, runBootstrap(t, committerBinary, bundlePath, org1Config), "Status: verified")
		assert.Contains(t, runBootstrap(t, committerBinary, bundlePath, org2Config), "Status: verified")
		org1Verification := runVerification(t, committerBinary, bundlePath, org1Config)
		org2Verification := runVerification(t, committerBinary, bundlePath, org2Config)
		assert.Equal(t, org1Verification, org2Verification, "organizations disagree on migration evidence")
		verifyDatabase(t, devnet, "committer-db", bundle, "multi_org_basic", anchorHex, anchor, "VERIFIED")
		verifyDatabase(t, devnet, "committer-org2-db", bundle, "multi_org_basic", anchorHex, anchor, "VERIFIED")

		assert.Contains(t, runActivation(t, committerBinary, bundlePath, org1Config), "Status: active")
		assert.Contains(t, runActivation(t, committerBinary, bundlePath, org2Config), "Status: active")
		assert.Equal(t, "ACTIVE", sql(t, devnet, "SELECT status FROM migration_record"))
		assert.Equal(t, "ACTIVE", sqlDB(t, devnet, "committer-org2-db", "SELECT status FROM migration_record"))
		startOrderers(t, devnet)
		composeBoth(t, devnet, "start", "committer-verifier", "committer-validator", "committer-coordinator", "committer-sidecar", "committer-query-service", "committer-org2-verifier", "committer-org2-validator", "committer-org2-coordinator", "committer-org2-sidecar", "committer-org2-query-service")
		submitPostActivationTransaction(t, devnet, "multi_org_basic", bundle.PublicState[0], "committer-db", "committer-org2-db")
	})

	t.Run("when a committer database is rebuilt, it reimports at the anchor before replaying later blocks", func(t *testing.T) {
		run(t, devnet, "make", "purge")
		t.Cleanup(func() { run(t, devnet, "make", "purge") })
		run(t, devnet, "make", "init")
		run(t, devnet, "make", "start")
		run(t, devnet, "make", "init-namespace-org1", "NS=recovery_basic")

		snapshotPath := filepath.Join(labRepo, "artifacts", "snapshots", "fabric-3.1.5", "block-3")
		bundlePath, bundle := exportBundle(t, migrateRepo, snapshotPath, "3.1.5", "basic", "recovery_basic")
		anchorHex, anchor := targetAnchor(t, devnet, "committer-db", "recovery_basic")
		configPath := writeBootstrapConfig(t, "committer-db:5432")

		freezeAtAnchor(t, devnet, freezeBinary, anchor)
		assert.Contains(t, runBootstrap(t, committerBinary, bundlePath, configPath), "Status: verified")
		assert.Contains(t, runActivation(t, committerBinary, bundlePath, configPath), "Status: active")
		startOrderers(t, devnet)
		compose(t, devnet, "start", "committer-verifier", "committer-validator", "committer-coordinator", "committer-sidecar", "committer-query-service")
		txID, value := submitPostActivationTransaction(t, devnet, "recovery_basic", bundle.PublicState[0], "committer-db")

		blockPaths := captureBlocks(t, devnet, anchor+1)
		laterBlockBytes, err := os.ReadFile(blockPaths[anchor+1])
		require.NoError(t, err)
		laterBlock := &common.Block{}
		require.NoError(t, proto.Unmarshal(laterBlockBytes, laterBlock))
		require.Equal(t, anchor+1, laterBlock.Header.Number)
		require.Len(t, laterBlock.Data.Data, 1)

		run(t, devnet, "make", "purge")
		run(t, devnet, "make", "start")
		waitForDatabaseAnchor(t, devnet, "committer-db", 0)
		freezeAtAnchor(t, devnet, freezeBinary, 0)
		compose(t, devnet, "start", "committer-verifier", "committer-validator", "committer-coordinator")
		replayBlocks(t, devnet, replayBinary, blockPaths[1:anchor+1])
		assert.Equal(t, anchorHex, sql(t, devnet, "SELECT encode(value, 'hex') FROM metadata WHERE convert_from(key, 'UTF8') = 'last committed block number'"))

		compose(t, devnet, "stop", "committer-verifier", "committer-validator", "committer-coordinator")
		assert.Contains(t, runBootstrap(t, committerBinary, bundlePath, configPath), "Status: verified")
		assert.Contains(t, runActivation(t, committerBinary, bundlePath, configPath), "Status: active")
		verifyDatabase(t, devnet, "committer-db", bundle, "recovery_basic", anchorHex, anchor, "ACTIVE")

		compose(t, devnet, "start", "committer-verifier", "committer-validator", "committer-coordinator")
		replayBlocks(t, devnet, replayBinary, blockPaths[anchor+1:])
		keyHex := hex.EncodeToString(bundle.PublicState[0].Key)
		assert.Equal(t, hex.EncodeToString(value), sql(t, devnet, "SELECT encode(value, 'hex') FROM ns_recovery_basic WHERE key = decode('"+keyHex+"', 'hex')"))
		assert.Equal(t, fmt.Sprint(int32(committerpb.Status_COMMITTED)), sql(t, devnet, "SELECT status FROM tx_status WHERE tx_id = decode('"+hex.EncodeToString([]byte(txID))+"', 'hex')"))
		assert.Equal(t, fmt.Sprint(anchor+1), fmt.Sprint(decodeAnchor(t, sql(t, devnet, "SELECT encode(value, 'hex') FROM metadata WHERE convert_from(key, 'UTF8') = 'last committed block number'"))))
		assert.Equal(t, fmt.Sprintf("ACTIVE:%d", anchor), sql(t, devnet, "SELECT status||':'||target_anchor FROM migration_record"))
	})
}

func exerciseMigration(t *testing.T, devnet, migrateRepo, committerBinary, freezeBinary, snapshotPath, version, sourceChannel, targetNamespace string) string {
	t.Helper()
	run(t, devnet, "make", "purge")
	t.Cleanup(func() { run(t, devnet, "make", "purge") })
	run(t, devnet, "make", "init")
	run(t, devnet, "make", "start")
	run(t, devnet, "make", "init-namespace-org1", "NS="+targetNamespace)

	bundlePath, bundle := exportBundle(t, migrateRepo, snapshotPath, version, "basic", targetNamespace)
	assert.Equal(t, sourceChannel, bundle.Manifest.Source.Channel)

	anchorHex, anchor := targetAnchor(t, devnet, "committer-db", targetNamespace)

	freezeAtAnchor(t, devnet, freezeBinary, anchor)
	configPath := writeBootstrapConfig(t, "committer-db:5432")
	first := runBootstrap(t, committerBinary, bundlePath, configPath)
	assert.Contains(t, first, "Status: verified")
	assert.Contains(t, first, fmt.Sprintf("Target anchor: %d", anchor))
	second := runBootstrap(t, committerBinary, bundlePath, configPath)
	assert.Contains(t, second, "Status: already verified")
	verified := runVerification(t, committerBinary, bundlePath, configPath)
	assert.Contains(t, verified, "Migration status: VERIFIED")
	assert.Contains(t, verified, "Target configuration SHA-256: "+sql(t, devnet, "SELECT encode(target_config_hash, 'hex') FROM migration_record"))
	assert.Contains(t, verified, "Namespace map SHA-256: "+sql(t, devnet, "SELECT encode(namespace_map_hash, 'hex') FROM migration_record"))
	assert.Contains(t, verified, "Target policies SHA-256: "+sql(t, devnet, "SELECT encode(target_policy_hash, 'hex') FROM migration_record"))
	assert.Contains(t, verified, "Integrity: verified")

	verifyDatabase(t, devnet, "committer-db", bundle, targetNamespace, anchorHex, anchor, "VERIFIED")
	activation := runActivation(t, committerBinary, bundlePath, configPath)
	assert.Contains(t, activation, "Status: active")
	again := runActivation(t, committerBinary, bundlePath, configPath)
	assert.Contains(t, again, "Status: already active")
	blocked := runBootstrapFailure(t, committerBinary, bundlePath, configPath)
	assert.Contains(t, blocked, "ACTIVE; snapshot import is disabled")
	activeVerification := runVerification(t, committerBinary, bundlePath, configPath)
	assert.Contains(t, activeVerification, "Migration status: ACTIVE")
	assert.Contains(t, activeVerification, "Integrity: verified")
	verifyDatabase(t, devnet, "committer-db", bundle, targetNamespace, anchorHex, anchor, "ACTIVE")
	startOrderers(t, devnet)
	compose(t, devnet, "start", "committer-verifier", "committer-validator", "committer-coordinator", "committer-sidecar", "committer-query-service")
	runVerification(t, committerBinary, bundlePath, configPath)
	verifyDatabase(t, devnet, "committer-db", bundle, targetNamespace, anchorHex, anchor, "ACTIVE")
	submitPostActivationTransaction(t, devnet, targetNamespace, bundle.PublicState[0], "committer-db")
	return bundle.MigrationID
}

func exportBundle(t *testing.T, migrateRepo, snapshotPath, version, sourceNamespace, targetNamespace string) (string, *genesisdata.File) {
	t.Helper()
	bundlePath := filepath.Join(t.TempDir(), targetNamespace+".fxgenesis")
	run(t, migrateRepo, "go", "run", "./cmd/fabric-x-migrate", "export",
		"--snapshot", snapshotPath,
		"--output", bundlePath,
		"--namespace", sourceNamespace+"="+targetNamespace,
		"--fabric-version", version,
	)
	run(t, migrateRepo, "go", "run", "./cmd/fabric-x-migrate", "verify-source", "--snapshot", snapshotPath, "--input", bundlePath)
	bundle, err := genesisdata.Read(bundlePath)
	require.NoError(t, err)
	require.NotEmpty(t, bundle.PublicState)
	assert.Equal(t, version, bundle.Manifest.Source.FabricVersion)
	return bundlePath, bundle
}

func decodeAnchor(t *testing.T, anchorHex string) uint64 {
	t.Helper()
	anchorBytes, err := hex.DecodeString(anchorHex)
	require.NoError(t, err)
	require.Len(t, anchorBytes, 8, "invalid target anchor %q", anchorHex)
	return binary.BigEndian.Uint64(anchorBytes)
}

func targetAnchor(t *testing.T, devnet, dbService, namespace string) (string, uint64) {
	t.Helper()
	var anchorHex string
	var anchor uint64
	require.Eventually(t, func() bool {
		anchorHex = sqlDB(t, devnet, dbService, "SELECT encode(value, 'hex') FROM metadata WHERE convert_from(key, 'UTF8') = 'last committed block number'")
		anchor = decodeAnchor(t, anchorHex)
		if sqlDB(t, devnet, dbService, "SELECT count(*) FROM ns__meta WHERE key = convert_to('"+namespace+"', 'UTF8')") != "1" {
			return false
		}
		heightBytes, err := hex.DecodeString(sqlDB(t, devnet, dbService, "SELECT encode(height, 'hex') FROM tx_status ORDER BY height DESC LIMIT 1"))
		require.NoError(t, err)
		height, _, err := servicepb.NewHeightFromBytes(heightBytes)
		require.NoError(t, err)
		return anchor == height.BlockNum
	}, time.Minute, time.Second)
	return anchorHex, anchor
}

func submitPostActivationTransaction(t *testing.T, devnet, namespace string, imported *genesisdata.StateRecord, dbServices ...string) (string, []byte) {
	t.Helper()
	identity := &ordererdial.IdentityConfig{
		MspID:  "Org1MSP",
		MSPDir: filepath.Join(devnet, "crypto", "peerOrganizations", "org1.example.com", "users", "User1@org1.example.com", "msp"),
	}
	policy := &workload.Policy{Scheme: workload.PolicySchemeMSP, MSPIdentities: []*ordererdial.IdentityConfig{identity}}
	builder, err := workload.NewTxBuilderFromPolicy(&workload.PolicyProfile{
		ChannelID: "mychannel",
		Identity:  identity,
		NamespacePolicies: map[string]*workload.Policy{
			committerpb.MetaNamespaceID: policy,
			namespace:                   policy,
		},
	}, nil)
	require.NoError(t, err)

	version := uint64(0)
	value := []byte("post-activation-value")
	tx := builder.MakeTx(&applicationpb.Tx{Namespaces: []*applicationpb.TxNamespace{{
		NsId:      namespace,
		NsVersion: 0,
		ReadWrites: []*applicationpb.ReadWrite{{
			Key: imported.Key, Version: &version, Value: value,
		}},
	}}})

	tlsCredentials, err := connection.NewClientTLSCredentials(connection.TLSConfig{
		Mode: connection.MutualTLSMode,
		CertPath: filepath.Join(devnet, "crypto", "peerOrganizations", "org1.example.com", "peers",
			"fxconfig.org1.example.com", "tls", "server.crt"),
		KeyPath: filepath.Join(devnet, "crypto", "peerOrganizations", "org1.example.com", "peers",
			"fxconfig.org1.example.com", "tls", "server.key"),
		CACertPaths: []string{filepath.Join(devnet, "crypto", "ordererOrganizations", "orderer-org-1", "msp",
			"tlscacerts", "tlsca.orderer-org-1-cert.pem")},
	})
	require.NoError(t, err)
	transportCredentials, err := connection.NewClientGRPCTransportCredentials(tlsCredentials)
	require.NoError(t, err)
	conn, err := grpc.NewClient("localhost:7050", grpc.WithTransportCredentials(transportCredentials))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	stream, err := ab.NewAtomicBroadcastClient(conn).Broadcast(ctx)
	require.NoError(t, err)
	require.NoError(t, stream.Send(&common.Envelope{Payload: tx.EnvelopePayload, Signature: tx.EnvelopeSignature}))
	response, err := stream.Recv()
	require.NoError(t, err)
	assert.Equal(t, common.Status_SUCCESS, response.Status)
	require.NoError(t, stream.CloseSend())

	keyHex := hex.EncodeToString(imported.Key)
	valueHex := hex.EncodeToString(value)
	txIDHex := hex.EncodeToString([]byte(tx.Id))
	for _, dbService := range dbServices {
		require.Eventually(t, func() bool {
			return sqlDB(t, devnet, dbService, "SELECT encode(value, 'hex') FROM ns_"+namespace+" WHERE key = decode('"+keyHex+"', 'hex')") == valueHex &&
				sqlDB(t, devnet, dbService, "SELECT status FROM tx_status WHERE tx_id = decode('"+txIDHex+"', 'hex')") == fmt.Sprint(int32(committerpb.Status_COMMITTED))
		}, time.Minute, time.Second)
		assert.Equal(t, "t", sqlDB(t, devnet, dbService, "SELECT version > 0 FROM ns_"+namespace+" WHERE key = decode('"+keyHex+"', 'hex')"))
		assert.Equal(t, "ACTIVE", sqlDB(t, devnet, dbService, "SELECT status FROM migration_record"))
	}
	return tx.Id, value
}

func cloneSnapshotWithChannel(t *testing.T, source, channel string) string {
	t.Helper()
	destination := t.TempDir()
	entries, err := os.ReadDir(source)
	require.NoError(t, err)
	for _, entry := range entries {
		require.False(t, entry.IsDir(), "snapshot fixture contains directory %s", entry.Name())
		data, err := os.ReadFile(filepath.Join(source, entry.Name()))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(destination, entry.Name()), data, 0o600))
	}

	signablePath := filepath.Join(destination, "_snapshot_signable_metadata.json")
	var signable map[string]any
	signableBytes, err := os.ReadFile(signablePath)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(signableBytes, &signable))
	signable["channel_name"] = channel
	signableBytes, err = json.MarshalIndent(signable, "", "    ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(signablePath, signableBytes, 0o600))

	additionalPath := filepath.Join(destination, "_snapshot_additional_metadata.json")
	var additional map[string]any
	additionalBytes, err := os.ReadFile(additionalPath)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(additionalBytes, &additional))
	hash := sha256.Sum256(signableBytes)
	additional["snapshot_hash"] = hex.EncodeToString(hash[:])
	additionalBytes, err = json.MarshalIndent(additional, "", "    ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(additionalPath, additionalBytes, 0o600))
	return destination
}

func runBootstrap(t *testing.T, binary, bundle, config string) string {
	t.Helper()
	return runCommand(t, offlineCommand(binary, bundle, config, "--init-from-snapshot"))
}

func runActivation(t *testing.T, binary, bundle, config string) string {
	t.Helper()
	return runCommand(t, offlineCommand(binary, bundle, config, "--activate-migration"))
}

func runVerification(t *testing.T, binary, bundle, config string) string {
	t.Helper()
	return runCommand(t, offlineCommand(binary, bundle, config, "--verify-migration"))
}

func runBootstrapFailure(t *testing.T, binary, bundle, config string) string {
	t.Helper()
	return runCommandFailure(t, offlineCommand(binary, bundle, config, "--init-from-snapshot"))
}

func offlineCommand(binary, bundle, config, operation string) *exec.Cmd {
	return exec.Command("docker", "run", "--rm", "--network", "fabric-x",
		"-v", binary+":/committer:ro",
		"-v", bundle+":/snapshot.fxgenesis:ro",
		"-v", config+":/config.yaml:ro",
		"busybox:latest", "/committer",
		operation, "/snapshot.fxgenesis", "--config", "/config.yaml",
	)
}

func verifyDatabase(t *testing.T, devnet, dbService string, bundle *genesisdata.File, targetNamespace, anchorHex string, anchor uint64, status string) {
	t.Helper()
	assert.Equal(t, fmt.Sprint(len(bundle.PublicState)), sqlDB(t, devnet, dbService, "SELECT count(*) FROM ns_"+targetNamespace))
	assert.Equal(t, fmt.Sprint(len(bundle.TransactionIDs)), sqlDB(t, devnet, dbService, "SELECT count(*) FROM migrated_tx_ids"))
	assert.Equal(t, "0:0", sqlDB(t, devnet, dbService, "SELECT min(version)||':'||max(version) FROM ns_"+targetNamespace))
	assert.Equal(t, anchorHex, sqlDB(t, devnet, dbService, "SELECT encode(value, 'hex') FROM metadata WHERE convert_from(key, 'UTF8') = 'last committed block number'"))
	assert.Equal(t, fmt.Sprintf("%s:%d:%d:%d", status, anchor, len(bundle.PublicState), len(bundle.TransactionIDs)), sqlDB(t, devnet, dbService, "SELECT status||':'||target_anchor||':'||public_state_count||':'||transaction_id_count FROM migration_record"))
	assert.Equal(t, bundle.MigrationID, sqlDB(t, devnet, dbService, "SELECT encode(migration_id, 'hex') FROM migration_record"))

	actualState := strings.Split(sqlDB(t, devnet, dbService, "SELECT encode(key, 'hex')||':'||encode(value, 'hex')||':'||version FROM ns_"+targetNamespace+" ORDER BY key"), "\n")
	expectedState := make([]string, len(bundle.PublicState))
	for i, record := range bundle.PublicState {
		expectedState[i] = fmt.Sprintf("%s:%s:0", hex.EncodeToString(record.Key), hex.EncodeToString(record.Value))
	}
	sort.Strings(expectedState)
	assert.Equal(t, expectedState, actualState, "target state differs from bundle")

	actualTxIDs := strings.Split(sqlDB(t, devnet, dbService, "SELECT encode(tx_id, 'hex') FROM migrated_tx_ids ORDER BY tx_id"), "\n")
	expectedTxIDs := make([]string, len(bundle.TransactionIDs))
	for i, record := range bundle.TransactionIDs {
		expectedTxIDs[i] = hex.EncodeToString([]byte(record.TransactionId))
	}
	sort.Strings(expectedTxIDs)
	assert.Equal(t, expectedTxIDs, actualTxIDs, "target transaction IDs differ from bundle")
}

func sql(t *testing.T, devnet, query string) string {
	t.Helper()
	return sqlDB(t, devnet, "committer-db", query)
}

func sqlDB(t *testing.T, devnet, dbService, query string) string {
	t.Helper()
	return strings.TrimSpace(composeBoth(t, devnet, "exec", "-T", dbService,
		"psql", "-v", "ON_ERROR_STOP=1", "-U", "sc_user", "-d", "sc_db", "-Atc", query))
}

func compose(t *testing.T, devnet string, args ...string) string {
	t.Helper()
	return run(t, devnet, "docker", append([]string{"compose", "-f", filepath.Join(devnet, "compose.yaml")}, args...)...)
}

func composeBoth(t *testing.T, devnet string, args ...string) string {
	t.Helper()
	return run(t, devnet, "docker", append([]string{"compose", "-f", filepath.Join(devnet, "compose.yaml"), "-f", filepath.Join(devnet, "compose.org2.yaml")}, args...)...)
}

func freezeAtAnchor(t *testing.T, devnet, freezeBinary string, anchor uint64) {
	t.Helper()
	stopOrderers(t, devnet)
	requireSidecarHeight(t, devnet, "localhost:4001", "org1", "fxconfig", anchor+1)
	requireCoordinatorAnchor(t, devnet, freezeBinary, "committer-coordinator:9001", "org1", "committer-sidecar", anchor+1)
	require.Equal(t, anchor, databaseAnchor(t, devnet, "committer-db"))
	compose(t, devnet, "stop", "committer-sidecar")
	compose(t, devnet, "stop", "committer-verifier", "committer-validator", "committer-coordinator", "committer-query-service")
}

func freezeBothAtAnchor(t *testing.T, devnet, freezeBinary string, anchor uint64) {
	t.Helper()
	stopOrderers(t, devnet)
	requireSidecarHeight(t, devnet, "localhost:4001", "org1", "fxconfig", anchor+1)
	requireSidecarHeight(t, devnet, "localhost:4002", "org2", "committer-org2-sidecar", anchor+1)
	requireCoordinatorAnchor(t, devnet, freezeBinary, "committer-coordinator:9001", "org1", "committer-sidecar", anchor+1)
	requireCoordinatorAnchor(t, devnet, freezeBinary, "committer-org2-coordinator:9001", "org2", "committer-org2-sidecar", anchor+1)
	require.Equal(t, anchor, databaseAnchor(t, devnet, "committer-db"))
	require.Equal(t, anchor, databaseAnchor(t, devnet, "committer-org2-db"))
	composeBoth(t, devnet, "stop", "committer-sidecar", "committer-org2-sidecar")
	composeBoth(t, devnet, "stop", "committer-verifier", "committer-validator", "committer-coordinator", "committer-query-service", "committer-org2-verifier", "committer-org2-validator", "committer-org2-coordinator", "committer-org2-query-service")
}

func requireSidecarHeight(t *testing.T, devnet, endpoint, org, clientPeer string, expected uint64) {
	t.Helper()
	peer := clientPeer + "." + org + ".example.com"
	tlsDirectory := filepath.Join(devnet, "crypto", "peerOrganizations", org+".example.com", "peers", peer, "tls")
	tlsCredentials, err := connection.NewClientTLSCredentials(connection.TLSConfig{
		Mode:        connection.MutualTLSMode,
		CertPath:    filepath.Join(tlsDirectory, "server.crt"),
		KeyPath:     filepath.Join(tlsDirectory, "server.key"),
		CACertPaths: []string{filepath.Join(devnet, "crypto", "peerOrganizations", org+".example.com", "msp", "tlscacerts", "tlsca."+org+".example.com-cert.pem")},
	})
	require.NoError(t, err)
	transportCredentials, err := connection.NewClientGRPCTransportCredentials(tlsCredentials)
	require.NoError(t, err)
	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(transportCredentials))
	require.NoError(t, err)
	defer conn.Close()
	info, err := committerpb.NewBlockQueryServiceClient(conn).GetBlockchainInfo(t.Context(), &emptypb.Empty{})
	require.NoError(t, err)
	require.Equal(t, expected, info.Height)
}

func requireCoordinatorAnchor(t *testing.T, devnet, binary, endpoint, org, clientPeer string, nextBlock uint64) {
	t.Helper()
	run(t, devnet, "docker", "run", "--rm", "--network", "fabric-x",
		"-v", binary+":/freezecheck:ro",
		"-v", filepath.Join(devnet, "crypto", "peerOrganizations", org+".example.com", "peers", clientPeer+"."+org+".example.com", "tls")+":/client-tls:ro",
		"-v", filepath.Join(devnet, "crypto", "peerOrganizations", org+".example.com", "msp", "tlscacerts", "tlsca."+org+".example.com-cert.pem")+":/org-tls-ca.pem:ro",
		"busybox:latest", "/freezecheck", endpoint, fmt.Sprint(nextBlock))
}

func databaseAnchor(t *testing.T, devnet, dbService string) uint64 {
	t.Helper()
	return decodeAnchor(t, sqlDB(t, devnet, dbService, "SELECT encode(value, 'hex') FROM metadata WHERE convert_from(key, 'UTF8') = 'last committed block number'"))
}

func waitForDatabaseAnchor(t *testing.T, devnet, dbService string, expected uint64) {
	t.Helper()
	require.Eventually(t, func() bool {
		command := exec.Command(
			"docker", "compose", "-f", filepath.Join(devnet, "compose.yaml"),
			"exec", "-T", dbService,
			"psql", "-v", "ON_ERROR_STOP=1", "-U", "sc_user", "-d", "sc_db", "-Atc",
			"SELECT encode(value, 'hex') FROM metadata WHERE convert_from(key, 'UTF8') = 'last committed block number'",
		)
		output, err := command.Output()
		if err != nil {
			return false
		}
		value, err := hex.DecodeString(strings.TrimSpace(string(output)))
		return err == nil && len(value) == 8 && binary.BigEndian.Uint64(value) == expected
	}, time.Minute, time.Second)
}

func stopOrderers(t *testing.T, devnet string) {
	t.Helper()
	compose(t, devnet, append([]string{"stop"}, ordererServices()...)...)
	require.Eventually(t, func() bool {
		conn, err := net.DialTimeout("tcp", "localhost:7050", 100*time.Millisecond)
		if err != nil {
			return true
		}
		_ = conn.Close()
		return false
	}, time.Minute, 100*time.Millisecond)
}

func startOrderers(t *testing.T, devnet string) {
	t.Helper()
	compose(t, devnet, append([]string{"start"}, ordererServices()...)...)
	require.Eventually(t, func() bool {
		conn, err := net.DialTimeout("tcp", "localhost:7050", 100*time.Millisecond)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	}, time.Minute, 100*time.Millisecond)
}

func ordererServices() []string {
	services := make([]string, 0, 16)
	for party := 1; party <= 4; party++ {
		for _, component := range []string{"router", "batcher", "consenter", "assembler"} {
			services = append(services, fmt.Sprintf("orderer-party%d-%s", party, component))
		}
	}
	return services
}

func writeBootstrapConfig(t *testing.T, dbEndpoint string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "committer-validator.yaml")
	config := fmt.Sprintf(`server:
  endpoint: :6001
  tls:
    mode: none
monitoring:
  endpoint: :2116
  tls:
    mode: none
database:
  endpoints:
    - %s
  username: sc_user
  password: sc_secret_pwd
  database: sc_db
  tls:
    mode: none
  max-connections: 10
  min-connections: 1
  load-balance: false
  table-pre-split-tablets: 0
  retry:
    initial-interval: 100ms
    randomization-factor: 0.1
    multiplier: 1.5
    max-interval: 1s
    max-elapsed-time: 30s
resource-limits:
  max-workers-for-preparer: 1
  max-workers-for-validator: 1
  max-workers-for-committer: 1
  min-transaction-batch-size: 1
  timeout-for-min-transaction-batch-size: 1s
logging:
  logSpec: error
`, dbEndpoint)
	require.NoError(t, os.WriteFile(path, []byte(config), 0o600))
	return path
}

func captureBlocks(t *testing.T, devnet string, lastBlock uint64) []string {
	t.Helper()
	tlsCredentials, err := connection.NewClientTLSCredentials(connection.TLSConfig{
		Mode: connection.MutualTLSMode,
		CertPath: filepath.Join(devnet, "crypto", "peerOrganizations", "org1.example.com", "peers",
			"fxconfig.org1.example.com", "tls", "server.crt"),
		KeyPath: filepath.Join(devnet, "crypto", "peerOrganizations", "org1.example.com", "peers",
			"fxconfig.org1.example.com", "tls", "server.key"),
		CACertPaths: []string{filepath.Join(devnet, "crypto", "peerOrganizations", "org1.example.com", "msp",
			"tlscacerts", "tlsca.org1.example.com-cert.pem")},
	})
	require.NoError(t, err)
	transportCredentials, err := connection.NewClientGRPCTransportCredentials(tlsCredentials)
	require.NoError(t, err)
	conn, err := grpc.NewClient("localhost:4001", grpc.WithTransportCredentials(transportCredentials))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	directory := t.TempDir()
	paths := make([]string, lastBlock+1)
	client := committerpb.NewBlockQueryServiceClient(conn)
	for blockNumber := uint64(0); blockNumber <= lastBlock; blockNumber++ {
		block, getErr := client.GetBlockByNumber(t.Context(), &committerpb.BlockNumber{Number: blockNumber})
		require.NoError(t, getErr)
		blockBytes, marshalErr := proto.Marshal(block)
		require.NoError(t, marshalErr)
		paths[blockNumber] = filepath.Join(directory, fmt.Sprintf("block-%d.pb", blockNumber))
		require.NoError(t, os.WriteFile(paths[blockNumber], blockBytes, 0o600))
	}
	return paths
}

func replayBlocks(t *testing.T, devnet, binary string, blockPaths []string) {
	t.Helper()
	require.NotEmpty(t, blockPaths)
	blockDirectory := filepath.Dir(blockPaths[0])
	args := []string{"run", "--rm", "--network", "fabric-x",
		"-v", binary + ":/replayblocks:ro",
		"-v", blockDirectory + ":/blocks:ro",
		"-v", filepath.Join(devnet, "crypto", "peerOrganizations", "org1.example.com", "peers", "committer-sidecar.org1.example.com", "tls") + ":/client-tls:ro",
		"-v", filepath.Join(devnet, "crypto", "peerOrganizations", "org1.example.com", "msp", "tlscacerts", "tlsca.org1.example.com-cert.pem") + ":/org-tls-ca.pem:ro",
		"busybox:latest", "/replayblocks", "committer-coordinator:9001"}
	for _, blockPath := range blockPaths {
		args = append(args, "/blocks/"+filepath.Base(blockPath))
	}
	run(t, devnet, "docker", args...)
}

func containerRunning(t *testing.T, name string) bool {
	t.Helper()
	command := exec.Command("docker", "inspect", "--format", "{{.State.Running}}", name)
	output, err := command.CombinedOutput()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(output)) == "true"
}

func run(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	command := exec.Command(name, args...)
	command.Dir = dir
	return runCommand(t, command)
}

func runCommand(t *testing.T, command *exec.Cmd) string {
	t.Helper()
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s failed\n%s", command.String(), output)
	return string(output)
}

func runCommandFailure(t *testing.T, command *exec.Cmd) string {
	t.Helper()
	output, err := command.CombinedOutput()
	require.Error(t, err, "%s unexpectedly succeeded\n%s", command.String(), output)
	return string(output)
}
