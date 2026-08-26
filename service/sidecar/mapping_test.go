/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package sidecar

import (
	"fmt"
	"testing"

	"github.com/hyperledger/fabric-lib-go/common/flogging"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/hyperledger/fabric-x-common/protoutil"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/utils"
	"github.com/hyperledger/fabric-x-committer/utils/retry"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

// testChannelID is a shared channel ID used by sidecar mapping/relay tests.
const testChannelID = "chan"

func BenchmarkMapOneBlock(b *testing.B) {
	flogging.ActivateSpec("fatal")
	txs := workload.GenerateTransactions(b, nil, b.N)
	block := workload.MapToOrdererBlock(1, txs)

	var txIDToHeight utils.SyncMap[string, servicepb.Height]
	b.ResetTimer()
	mappedBlock, err := mapBlock(block, &txIDToHeight)
	b.StopTimer()
	test.ReportTxPerSecond(b)
	require.NoError(b, err, "This can never occur unless there is a bug in the relay.")
	require.NotNil(b, mappedBlock)
}

func BenchmarkMapBlockSize(b *testing.B) {
	flogging.ActivateSpec("fatal")
	for _, blockSize := range []int{100, 1000, 5000, 10000} {
		b.Run(fmt.Sprintf("blockSize=%d", blockSize), func(b *testing.B) {
			// b.N is the number of transactions; blockSize is only the work
			// granularity. We split b.N transactions into blocks of at most
			// blockSize (the final block may be smaller), so ns/op and tx/s are
			// reported per transaction, independent of the block size.
			allTxs := workload.GenerateTransactions(b, nil, b.N)
			blocks := make([]*common.Block, 0, (b.N+blockSize-1)/blockSize)
			for off := 0; off < b.N; off += blockSize {
				blocks = append(blocks, workload.MapToOrdererBlock(
					uint64(len(blocks)), allTxs[off:min(off+blockSize, b.N)],
				))
			}

			b.ResetTimer()
			for _, blk := range blocks {
				var txIDToHeight utils.SyncMap[string, servicepb.Height]
				if _, err := mapBlock(blk, &txIDToHeight); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			test.ReportTxPerSecond(b)
		})
	}
}

func TestBlockMapping(t *testing.T) {
	t.Parallel()
	txb := &workload.TxBuilder{ChannelID: testChannelID}
	txs, expected := MalformedTxTestCases(txb)
	expectedBlockSize := 0
	expectedRejected := 0
	for i, e := range expected {
		if !IsStatusStoredInDB(e) {
			continue
		}
		expected[i] = statusNotYetValidated
		expectedBlockSize++
		if e != committerpb.Status_COMMITTED {
			expectedRejected++
		}
	}
	lgTX := txb.MakeTx(txs[0].Tx)
	txs = append(txs, lgTX)
	expected = append(expected, committerpb.Status_REJECTED_DUPLICATE_TX_ID)

	var txIDToHeight utils.SyncMap[string, servicepb.Height]
	txIDToHeight.Store(lgTX.Id, servicepb.Height{})

	block := workload.MapToOrdererBlock(1, txs)
	mappedBlock, err := mapBlock(block, &txIDToHeight)
	require.NoError(t, err, "This can never occur unless there is a bug in the relay.")

	require.NotNil(t, mappedBlock)
	require.NotNil(t, mappedBlock.block)
	require.NotNil(t, mappedBlock.withStatus)

	require.Equal(t, block, mappedBlock.withStatus.block)
	require.Equal(t, block.Header.Number, mappedBlock.blockNumber)
	require.Equal(t, expected, mappedBlock.withStatus.txStatus)

	require.Equal(t, expectedBlockSize+1, txIDToHeight.Count())
	require.Len(t, mappedBlock.block.Txs, expectedBlockSize-expectedRejected)
	require.Len(t, mappedBlock.block.Rejected, expectedRejected)
	//nolint:gosec // int -> int32
	require.Equal(t, int32(expectedBlockSize), mappedBlock.withStatus.pendingCount.Load())
}

// TestConfigTxMapping verifies where the TX ID of a config TX comes from. The outer envelope of a
// config block that the ordering service creates for a config update is generated and signed by
// the consensus leader, so its TX ID is not the submitting client's; the client's TX is nested in
// ConfigEnvelope.LastUpdate. The outer TX ID is used only when there is no nested update at all, in
// a bootstrap (genesis) config block: it never stands in for a nested update the TX ID cannot be
// taken from, and the committer never computes an ID of its own, so either case fails the block.
// See https://github.com/hyperledger/fabric-x-committer/issues/752.
func TestConfigTxMapping(t *testing.T) {
	t.Parallel()

	const (
		userTxID      = "user-config-tx-id"
		consenterTxID = "consenter-config-tx-id"
		bootstrapTxID = "bootstrap-config-tx-id"
		// Errors reported for a nested config update the TX ID cannot be taken from.
		noNestedTxIDError     = "no TX ID in the config update"
		unreadableNestedError = "error parsing the config update envelope"
	)

	// Success cases.
	for _, tc := range []struct {
		name         string
		parts        configTxParts
		expectedTxID string
	}{
		{
			name: "config update: outer envelope has no TX ID, so the nested user TX ID is used",
			parts: configTxParts{
				lastUpdate: configUpdateForTest(t, userTxID),
			},
			expectedTxID: userTxID,
		},
		{
			name: "config update: the nested user TX ID overrides the consenter's TX ID",
			parts: configTxParts{
				outerTxID:  consenterTxID,
				lastUpdate: configUpdateForTest(t, userTxID),
			},
			expectedTxID: userTxID,
		},
		{
			name: "bootstrap block: no nested update, so the outer TX ID is used",
			parts: configTxParts{
				outerTxID: bootstrapTxID,
			},
			expectedTxID: bootstrapTxID,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			configEnv := configTxForTest(t, tc.parts)

			var txIDToHeight utils.SyncMap[string, servicepb.Height]
			mappedBlock, err := mapBlock(configBlockForTest(configEnv), &txIDToHeight)
			require.NoError(t, err)
			require.NotNil(t, mappedBlock)

			require.True(t, mappedBlock.isConfig)
			require.Empty(t, mappedBlock.block.Rejected)
			require.Equal(t, []committerpb.Status{statusNotYetValidated}, mappedBlock.withStatus.txStatus)

			require.Len(t, mappedBlock.block.Txs, 1)
			test.RequireProtoEqual(t, &servicepb.TxWithRef{
				Ref:     committerpb.NewTxRef(tc.expectedTxID, 1, 0),
				Content: configTx(configEnv),
			}, mappedBlock.block.Txs[0])

			// The TX ID must be tracked so the submitting client can be notified.
			height, ok := txIDToHeight.Load(tc.expectedTxID)
			require.True(t, ok)
			require.Equal(t, servicepb.Height{BlockNum: 1, TxNum: 0}, height)
		})
	}

	// Failure cases. A config TX has already been validated and applied by the ordering service,
	// so the committer cannot reject it; a config TX it cannot process must fail the block instead,
	// to be re-fetched from another source.
	for _, tc := range []struct {
		name                 string
		parts                configTxParts
		expectedErrorMessage string
	}{
		{
			name:                 "bootstrap block with no outer TX ID",
			parts:                configTxParts{},
			expectedErrorMessage: "no TX ID in the config TX",
		},
		{
			name: "nested update with no TX ID",
			parts: configTxParts{
				lastUpdate: configUpdateForTest(t, ""),
			},
			expectedErrorMessage: noNestedTxIDError,
		},
		{
			name: "nested update with no TX ID, which the outer TX ID does not stand in for",
			parts: configTxParts{
				outerTxID:  consenterTxID,
				lastUpdate: configUpdateForTest(t, ""),
			},
			expectedErrorMessage: noNestedTxIDError,
		},
		{
			name: "unreadable nested update",
			parts: configTxParts{
				lastUpdate: &common.Envelope{},
			},
			expectedErrorMessage: unreadableNestedError,
		},
		{
			name: "unreadable nested update, which the outer TX ID does not stand in for",
			parts: configTxParts{
				outerTxID:  consenterTxID,
				lastUpdate: &common.Envelope{},
			},
			expectedErrorMessage: unreadableNestedError,
		},
		{
			name: "channel config that cannot be parsed into a bundle",
			parts: configTxParts{
				lastUpdate:    configUpdateForTest(t, userTxID),
				invalidConfig: true,
			},
			expectedErrorMessage: "error parsing config",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			block := configBlockForTest(configTxForTest(t, tc.parts))

			var txIDToHeight utils.SyncMap[string, servicepb.Height]
			_, err := mapBlock(block, &txIDToHeight)
			require.ErrorContains(t, err, tc.expectedErrorMessage)
			// The block must be re-fetched, possibly from another orderer, rather than committed.
			require.ErrorIs(t, err, retry.ErrBackOff)
		})
	}
}

// TestConfigTxDuplicateID verifies that a config TX whose TX ID is already in flight fails the
// block, rather than being rejected as a duplicate like a data TX would be. The ordering service
// has already applied the config, so the committer must apply it too.
func TestConfigTxDuplicateID(t *testing.T) {
	t.Parallel()

	const userTxID = "user-config-tx-id"
	block := configBlockForTest(configTxForTest(t, configTxParts{
		lastUpdate: configUpdateForTest(t, userTxID),
	}))

	var txIDToHeight utils.SyncMap[string, servicepb.Height]
	txIDToHeight.Store(userTxID, *servicepb.NewHeight(0, 0))
	_, err := mapBlock(block, &txIDToHeight)
	require.ErrorContains(t, err, "duplicate TX ID ["+userTxID+"]")
	// The block must be re-fetched, by which time the TX holding the ID may have been processed.
	require.ErrorIs(t, err, retry.ErrBackOff)
}

func TestSystemNamespaceFormValidation(t *testing.T) {
	t.Parallel()

	const ordinaryNsID = "ordinary"
	heightKey := servicepb.NewHeight(7, 3).ToBytes()
	heightKeyWithTrailingBytes := append(append([]byte{}, heightKey...), []byte("junk")...)

	for _, tc := range []struct {
		name                string
		tx                  *applicationpb.Tx
		expectedStatus      committerpb.Status
		expectedHasSnapshot bool
	}{
		{
			name: "marker-only snapshot namespace is valid",
			tx: &applicationpb.Tx{
				Namespaces:   []*applicationpb.TxNamespace{{NsId: committerpb.SnapshotNamespaceID}},
				Endorsements: dummyEndorsements(1),
			},
			expectedStatus:      statusNotYetValidated,
			expectedHasSnapshot: true,
		},
		{
			name: "snapshot namespace with reads-only is malformed",
			tx: &applicationpb.Tx{
				Namespaces: []*applicationpb.TxNamespace{{
					NsId:      committerpb.SnapshotNamespaceID,
					ReadsOnly: []*applicationpb.Read{{Key: []byte("key")}},
				}},
				Endorsements: dummyEndorsements(1),
			},
			expectedStatus: committerpb.Status_MALFORMED_SNAPSHOT_NOT_MARKER_ONLY,
		},
		{
			name: "snapshot namespace with read-writes is malformed",
			tx: &applicationpb.Tx{
				Namespaces: []*applicationpb.TxNamespace{{
					NsId:       committerpb.SnapshotNamespaceID,
					ReadWrites: []*applicationpb.ReadWrite{{Key: []byte("key"), Value: []byte("value")}},
				}},
				Endorsements: dummyEndorsements(1),
			},
			expectedStatus: committerpb.Status_MALFORMED_SNAPSHOT_NOT_MARKER_ONLY,
		},
		{
			name: "snapshot namespace with blind-writes is malformed",
			tx: &applicationpb.Tx{
				Namespaces: []*applicationpb.TxNamespace{{
					NsId:        committerpb.SnapshotNamespaceID,
					BlindWrites: []*applicationpb.Write{{Key: []byte("key"), Value: []byte("value")}},
				}},
				Endorsements: dummyEndorsements(1),
			},
			expectedStatus: committerpb.Status_MALFORMED_SNAPSHOT_NOT_MARKER_ONLY,
		},
		{
			name: "snapshot namespace mixed with ordinary namespace is malformed",
			tx: &applicationpb.Tx{
				Namespaces: []*applicationpb.TxNamespace{
					{NsId: committerpb.SnapshotNamespaceID},
					{NsId: ordinaryNsID, BlindWrites: []*applicationpb.Write{{Key: []byte("key")}}},
				},
				Endorsements: dummyEndorsements(2),
			},
			expectedStatus: committerpb.Status_MALFORMED_SYSTEM_TX_NOT_STANDALONE,
		},
		{
			name: "ordinary empty namespace is malformed with no writes",
			tx: &applicationpb.Tx{
				Namespaces:   []*applicationpb.TxNamespace{{NsId: ordinaryNsID}},
				Endorsements: dummyEndorsements(1),
			},
			expectedStatus: committerpb.Status_MALFORMED_NO_WRITES,
		},
		{
			name: "checkpoint namespace with height key is valid",
			tx: &applicationpb.Tx{
				Namespaces: []*applicationpb.TxNamespace{{
					NsId:       committerpb.CheckpointNamespaceID,
					ReadWrites: []*applicationpb.ReadWrite{{Key: heightKey, Value: []byte("checkpoint")}},
				}},
				Endorsements: dummyEndorsements(1),
			},
			expectedStatus: statusNotYetValidated,
		},
		{
			name: "checkpoint namespace with non-height key is malformed",
			tx: &applicationpb.Tx{
				Namespaces: []*applicationpb.TxNamespace{{
					NsId:       committerpb.CheckpointNamespaceID,
					ReadWrites: []*applicationpb.ReadWrite{{Key: []byte("not-height"), Value: []byte("checkpoint")}},
				}},
				Endorsements: dummyEndorsements(1),
			},
			expectedStatus: committerpb.Status_MALFORMED_CHECKPOINT_INVALID_KEY,
		},
		{
			name: "checkpoint namespace with height key plus trailing bytes is malformed",
			tx: &applicationpb.Tx{
				Namespaces: []*applicationpb.TxNamespace{{
					NsId: committerpb.CheckpointNamespaceID,
					ReadWrites: []*applicationpb.ReadWrite{{
						Key:   heightKeyWithTrailingBytes,
						Value: []byte("checkpoint"),
					}},
				}},
				Endorsements: dummyEndorsements(1),
			},
			expectedStatus: committerpb.Status_MALFORMED_CHECKPOINT_INVALID_KEY,
		},
		{
			name: "checkpoint namespace with read-only is malformed",
			tx: &applicationpb.Tx{
				Namespaces: []*applicationpb.TxNamespace{{
					NsId:       committerpb.CheckpointNamespaceID,
					ReadsOnly:  []*applicationpb.Read{{Key: []byte("other-key")}},
					ReadWrites: []*applicationpb.ReadWrite{{Key: heightKey, Value: []byte("checkpoint")}},
				}},
				Endorsements: dummyEndorsements(1),
			},
			expectedStatus: committerpb.Status_MALFORMED_CHECKPOINT_INVALID_KEY,
		},
		{
			name: "checkpoint namespace with blind write is malformed",
			tx: &applicationpb.Tx{
				Namespaces: []*applicationpb.TxNamespace{{
					NsId:        committerpb.CheckpointNamespaceID,
					ReadWrites:  []*applicationpb.ReadWrite{{Key: heightKey, Value: []byte("checkpoint")}},
					BlindWrites: []*applicationpb.Write{{Key: []byte("key"), Value: []byte("value")}},
				}},
				Endorsements: dummyEndorsements(1),
			},
			expectedStatus: committerpb.Status_MALFORMED_CHECKPOINT_INVALID_KEY,
		},
		{
			name: "checkpoint namespace with more than one read-write is malformed",
			tx: &applicationpb.Tx{
				Namespaces: []*applicationpb.TxNamespace{{
					NsId: committerpb.CheckpointNamespaceID,
					ReadWrites: []*applicationpb.ReadWrite{
						{Key: heightKey, Value: []byte("checkpoint")},
						{Key: []byte("other-key"), Value: []byte("checkpoint")},
					},
				}},
				Endorsements: dummyEndorsements(1),
			},
			expectedStatus: committerpb.Status_MALFORMED_CHECKPOINT_INVALID_KEY,
		},
		{
			name: "checkpoint namespace with no read-write is malformed",
			tx: &applicationpb.Tx{
				Namespaces: []*applicationpb.TxNamespace{{
					NsId: committerpb.CheckpointNamespaceID,
				}},
				Endorsements: dummyEndorsements(1),
			},
			expectedStatus: committerpb.Status_MALFORMED_CHECKPOINT_INVALID_KEY,
		},
		{
			name: "checkpoint namespace mixed with ordinary namespace is malformed",
			tx: &applicationpb.Tx{
				Namespaces: []*applicationpb.TxNamespace{
					{
						NsId:       committerpb.CheckpointNamespaceID,
						ReadWrites: []*applicationpb.ReadWrite{{Key: heightKey, Value: []byte("checkpoint")}},
					},
					{NsId: ordinaryNsID, BlindWrites: []*applicationpb.Write{{Key: []byte("key")}}},
				},
				Endorsements: dummyEndorsements(2),
			},
			expectedStatus: committerpb.Status_MALFORMED_SYSTEM_TX_NOT_STANDALONE,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tc.expectedStatus, verifyTxForm(tc.tx))

			txb := &workload.TxBuilder{ChannelID: testChannelID}
			block := workload.MapToOrdererBlock(1, []*servicepb.LoadGenTx{txb.MakeTx(tc.tx)})

			var txIDToHeight utils.SyncMap[string, servicepb.Height]
			mappedBlock, err := mapBlock(block, &txIDToHeight)
			require.NoError(t, err)
			require.NotNil(t, mappedBlock)
			require.Equal(t, tc.expectedHasSnapshot, mappedBlock.snapshotTx != nil)
			if tc.expectedHasSnapshot {
				// The snapshot TX is kept separate from block.Txs; see the snapshotTx field
				// comment in mapping.go.
				require.Equal(
					t,
					committerpb.SnapshotNamespaceID,
					mappedBlock.snapshotTx.Content.Namespaces[0].NsId,
				)
				require.Empty(t, mappedBlock.block.Txs)
			}
		})
	}
}

// TestDuplicateSnapshotInBlock verifies that when a block contains more than one snapshot TX,
// only the first is accepted and the rest are rejected with
// REJECTED_DUPLICATE_SNAPSHOT_IN_BLOCK (a stored status), regardless of the first's outcome.
func TestDuplicateSnapshotInBlock(t *testing.T) {
	t.Parallel()

	snapshotTx := func() *applicationpb.Tx {
		return &applicationpb.Tx{
			Namespaces:   []*applicationpb.TxNamespace{{NsId: committerpb.SnapshotNamespaceID}},
			Endorsements: dummyEndorsements(1),
		}
	}
	regularTx := func() *applicationpb.Tx {
		return &applicationpb.Tx{
			Namespaces: []*applicationpb.TxNamespace{{
				NsId:        "ns",
				BlindWrites: []*applicationpb.Write{{Key: []byte("key")}},
			}},
			Endorsements: dummyEndorsements(1),
		}
	}

	txb := &workload.TxBuilder{ChannelID: testChannelID}
	// Block layout: [regular, snapshot#0 (accepted), regular, snapshot#1 (rejected)].
	block := workload.MapToOrdererBlock(1, []*servicepb.LoadGenTx{
		txb.MakeTx(regularTx()),
		txb.MakeTx(snapshotTx()),
		txb.MakeTx(regularTx()),
		txb.MakeTx(snapshotTx()),
	})

	var txIDToHeight utils.SyncMap[string, servicepb.Height]
	mappedBlock, err := mapBlock(block, &txIDToHeight)
	require.NoError(t, err)
	require.NotNil(t, mappedBlock)

	// Only the first snapshot is accepted and is kept separate from block.Txs (see the
	// snapshotTx field comment in mapping.go): block.Txs holds only the two regular TXs.
	require.NotNil(t, mappedBlock.snapshotTx)
	require.Equal(
		t,
		committerpb.SnapshotNamespaceID,
		mappedBlock.snapshotTx.Content.Namespaces[0].NsId,
	)
	require.Len(t, mappedBlock.block.Txs, 2)

	// The second snapshot is rejected with the dedicated stored status.
	require.Len(t, mappedBlock.block.Rejected, 1)
	require.Equal(
		t,
		committerpb.Status_REJECTED_DUPLICATE_SNAPSHOT_IN_BLOCK,
		mappedBlock.block.Rejected[0].Status,
	)
	require.Equal(t, uint32(3), mappedBlock.block.Rejected[0].Ref.TxNum)
}

// configTxParts describes the identifying fields of a config TX. Which of them are populated
// depends on who created the config block: the ordering service, when it applies a config update
// submitted by a client, or the bootstrap tooling, for a genesis block.
type configTxParts struct {
	// outerTxID is the outer envelope's TX ID. For a config update, it is the consensus leader's.
	outerTxID string
	// lastUpdate is the client's config-update TX, nested in ConfigEnvelope.LastUpdate.
	// It is nil in a genesis config block, which no client submitted.
	lastUpdate *common.Envelope
	// invalidConfig drops the channel config, so the config TX cannot be parsed into a bundle.
	invalidConfig bool
	// baseEnvelope is the config TX envelope whose channel config is reused. A config block is
	// generated when it is nil. Tests that feed the config TX to a running sidecar pass the
	// channel's own config TX here, so the config it carries matches the sidecar's channel.
	baseEnvelope []byte
}

// configTxForTest builds a config TX envelope holding the given identifying fields. Its channel
// config is taken from an existing config TX, so it stays valid (unless invalidConfig is set)
// without the test having to construct one.
func configTxForTest(t *testing.T, p configTxParts) []byte {
	t.Helper()
	baseEnvelope := p.baseEnvelope
	if baseEnvelope == nil {
		baseEnvelope = createConfigBlockForTest(t).Data.Data[0]
	}
	env, err := protoutil.UnmarshalEnvelope(baseEnvelope)
	require.NoError(t, err)
	payload, err := protoutil.UnmarshalPayload(env.Payload)
	require.NoError(t, err)
	channelHdr, err := protoutil.UnmarshalChannelHeader(payload.Header.ChannelHeader)
	require.NoError(t, err)

	channelHdr.TxId = p.outerTxID
	payload.Header.ChannelHeader = marshalForTest(t, channelHdr)

	configEnv, err := protoutil.UnmarshalConfigEnvelope(payload.Data)
	require.NoError(t, err)
	configEnv.LastUpdate = p.lastUpdate
	if p.invalidConfig {
		configEnv.Config = nil
	}
	payload.Data = marshalForTest(t, configEnv)

	env.Payload = marshalForTest(t, payload)
	return marshalForTest(t, env)
}

// configUpdateForTest creates the client's config-update TX as it appears in
// ConfigEnvelope.LastUpdate. It carries only the channel header the committer reads the TX ID from;
// the config update itself is applied by the ordering service and is irrelevant to the mapping.
func configUpdateForTest(t *testing.T, txID string) *common.Envelope {
	t.Helper()
	return &common.Envelope{Payload: marshalForTest(t, &common.Payload{
		Header: &common.Header{
			ChannelHeader: marshalForTest(t, &common.ChannelHeader{
				Type:      int32(common.HeaderType_CONFIG_UPDATE),
				ChannelId: testChannelID,
				TxId:      txID,
			}),
		},
	})}
}

// configBlockForTest wraps a config TX envelope in block number one, as the ordering service
// delivers it: a config TX is always alone in its block.
func configBlockForTest(configEnv []byte) *common.Block {
	return &common.Block{
		Header: &common.BlockHeader{Number: 1},
		Data:   &common.BlockData{Data: [][]byte{configEnv}},
	}
}

func marshalForTest(t *testing.T, m proto.Message) []byte {
	t.Helper()
	value, err := proto.Marshal(m)
	require.NoError(t, err)
	return value
}
