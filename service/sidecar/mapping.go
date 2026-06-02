/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package sidecar

import (
	"fmt"
	"sync/atomic"
	"unicode/utf8"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"go.uber.org/zap/zapcore"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/service/verifier/policy"
	"github.com/hyperledger/fabric-x-committer/utils"
	"github.com/hyperledger/fabric-x-committer/utils/serialization"
)

type (
	blockMappingResult struct {
		blockNumber uint64
		block       *servicepb.CoordinatorBatch
		withStatus  *blockWithStatus
		isConfig    bool
		// txIDToHeight is a reference to the relay map.
		txIDToHeight *utils.SyncMap[string, servicepb.Height]
	}

	blockWithStatus struct {
		block        *common.Block
		txStatus     []committerpb.Status
		pendingCount atomic.Int32

		// Fields for StreamAllTransactions support
		blockNumber uint64                 // Block number
		txs         []*servicepb.TxWithRef // Transaction content (from coordinatorBatch.Txs)
	}
)

const (
	statusNotYetValidated = committerpb.Status_STATUS_UNSPECIFIED
	statusIdx             = int(common.BlockMetadataIndex_TRANSACTIONS_FILTER)
)

func mapBlock(block *common.Block, txIDToHeight *utils.SyncMap[string, servicepb.Height]) (*blockMappingResult, error) {
	// Prepare block's metadata.
	if block.Metadata == nil {
		block.Metadata = &common.BlockMetadata{}
	}
	metadataSize := len(block.Metadata.Metadata)
	if metadataSize <= statusIdx {
		block.Metadata.Metadata = append(block.Metadata.Metadata, make([][]byte, statusIdx+1-metadataSize)...)
	}

	blockNumber := block.Header.Number

	if block.Data == nil {
		logger.Warnf("Received a block [%d] without data", block.Header.Number)
		return &blockMappingResult{
			blockNumber: blockNumber,
			block:       &servicepb.CoordinatorBatch{},
			withStatus: &blockWithStatus{
				block:       block,
				blockNumber: blockNumber,
			},
			txIDToHeight: txIDToHeight,
		}, nil
	}

	txCount := len(block.Data.Data)
	mappedBlock := &blockMappingResult{
		blockNumber: blockNumber,
		block: &servicepb.CoordinatorBatch{
			Txs:      make([]*servicepb.TxWithRef, 0, txCount),
			Rejected: make([]*committerpb.TxStatus, 0, txCount),
		},
		withStatus: &blockWithStatus{
			block:       block,
			txStatus:    make([]committerpb.Status, txCount),
			txs:         make([]*servicepb.TxWithRef, txCount),
			blockNumber: blockNumber,
		},
		txIDToHeight: txIDToHeight,
	}
	mappedBlock.withStatus.pendingCount.Store(int32(txCount)) //nolint:gosec // int -> int32

	for msgIndex, msg := range block.Data.Data {
		logger.Debugf("Mapping transaction [blk,tx] = [%d,%d]", blockNumber, msgIndex)
		err := mappedBlock.mapMessage(uint32(msgIndex), msg) //nolint:gosec // int -> uint32.
		if err != nil {
			// This can never occur unless there is a bug in the relay.
			return nil, err
		}
	}
	return mappedBlock, nil
}

func (b *blockMappingResult) mapMessage(msgIndex uint32, msg []byte) error {
	// UnwrapEnvelopeLite extracts only HeaderType, TxID, and Data from the envelope
	// by scanning the protobuf wire format directly. Unlike UnwrapEnvelope, which
	// fully deserializes all nested proto messages and validates every field, this
	// skips unused ChannelHeader fields (version, timestamp, channel_id, epoch,
	// extension, tls_cert_hash) and the Header's signature_header. Corruption in
	// those fields will go undetected. This is acceptable because the committer
	// does not use them, and for the same reason, they are not validated in the
	// sidecar. TODO: remove unused fields from the ChannelHeader proto.
	ref := committerpb.NewTxRef("", b.blockNumber, msgIndex)
	envLite, envErr := serialization.UnwrapEnvelopeLite(msg)
	if envErr != nil {
		return b.rejectNonDBStatusTx(ref, committerpb.Status_MALFORMED_BAD_ENVELOPE, envErr.Error())
	}
	if envLite.TxID == "" || !utf8.ValidString(envLite.TxID) {
		return b.rejectNonDBStatusTx(ref, committerpb.Status_MALFORMED_MISSING_TX_ID, "no TX ID")
	}
	ref.TxId = envLite.TxID

	switch common.HeaderType(envLite.HeaderType) {
	case common.HeaderType_CONFIG:
		if err := policy.ValidateConfigTx(msg); err != nil {
			return b.rejectTx(ref, committerpb.Status_MALFORMED_CONFIG_TX_INVALID, err.Error())
		}
		b.isConfig = true
		return b.appendTx(ref, configTx(msg))
	case common.HeaderType_MESSAGE:
		tx, err := serialization.UnmarshalTx(envLite.Data)
		if err != nil {
			return b.rejectTx(ref, committerpb.Status_MALFORMED_BAD_ENVELOPE_PAYLOAD, err.Error())
		}
		if status := verifyTxForm(tx); status != statusNotYetValidated {
			return b.rejectTx(ref, status, "malformed tx")
		}
		return b.appendTx(ref, tx)
	default:
		return b.rejectTx(ref, committerpb.Status_MALFORMED_UNSUPPORTED_ENVELOPE_PAYLOAD,
			"unsupported message type: "+common.HeaderType(envLite.HeaderType).String())
	}
}

func (b *blockMappingResult) appendTx(ref *committerpb.TxRef, tx *applicationpb.Tx) error {
	if idAlreadyExists, err := b.addTxIDMapping(ref); idAlreadyExists || err != nil {
		return err
	}
	txWithRef := &servicepb.TxWithRef{Ref: ref, Content: tx}
	b.block.Txs = append(b.block.Txs, txWithRef)
	b.withStatus.txs[ref.TxNum] = txWithRef
	debugTx(ref, "included: %s", ref.TxId)
	return nil
}

func (b *blockMappingResult) rejectTx(ref *committerpb.TxRef, status committerpb.Status, reason string) error {
	if !IsStatusStoredInDB(status) {
		return b.rejectNonDBStatusTx(ref, status, reason)
	}
	if idAlreadyExists, err := b.addTxIDMapping(ref); idAlreadyExists || err != nil {
		return err
	}
	b.block.Rejected = append(b.block.Rejected, &committerpb.TxStatus{Ref: ref, Status: status})
	b.withStatus.txs[ref.TxNum] = &servicepb.TxWithRef{Ref: ref}
	debugTx(ref, "rejected: %s (%s)", &status, reason)
	return nil
}

// rejectNonDBStatusTx is used to reject with statuses that are not stored in the state DB.
// Namely, statuses for cases where we don't have a TX ID, or there is a TX ID duplication.
// For such cases, no notification will be given by the notification service.
func (b *blockMappingResult) rejectNonDBStatusTx(
	ref *committerpb.TxRef, status committerpb.Status, reason string,
) error {
	if IsStatusStoredInDB(status) {
		// This can never occur unless there is a bug in the relay.
		return errors.Newf("[BUG] status should be stored [blk:%d,num:%d]: %s", b.blockNumber, ref.TxNum, &status)
	}
	err := b.withStatus.setFinalStatus(ref.TxNum, status)
	if err != nil {
		return err
	}
	b.withStatus.txs[ref.TxNum] = &servicepb.TxWithRef{Ref: ref}
	debugTx(ref, "excluded: %s (%s)", &status, reason)
	return nil
}

func (b *blockMappingResult) addTxIDMapping(ref *committerpb.TxRef) (
	idAlreadyExists bool, err error,
) {
	_, idAlreadyExists = b.txIDToHeight.LoadOrStore(ref.TxId, *servicepb.NewHeightFromTxRef(ref))
	if idAlreadyExists {
		err = b.rejectNonDBStatusTx(ref, committerpb.Status_REJECTED_DUPLICATE_TX_ID, "duplicate tx")
	}
	return idAlreadyExists, err
}

func (b *blockWithStatus) setFinalStatus(txNum uint32, status committerpb.Status) error {
	if b.txStatus[txNum] != statusNotYetValidated {
		// This can never occur unless there is a bug in the relay or the coordinator.
		return errors.Newf("two results for a TX [blockNum: %d, txNum: %d]", b.block.Header.Number, txNum)
	}
	b.txStatus[txNum] = status
	b.pendingCount.Add(-1)
	return nil
}

func (b *blockWithStatus) setStatusMetadataInBlock() {
	statusMetadata := make([]byte, len(b.txStatus))
	for i, s := range b.txStatus {
		statusMetadata[i] = byte(s)
	}
	b.block.Metadata.Metadata[statusIdx] = statusMetadata
}

// IsStatusStoredInDB returns true if the given status code can be stored in the state DB.
func IsStatusStoredInDB(status committerpb.Status) bool {
	switch status {
	case committerpb.Status_MALFORMED_BAD_ENVELOPE,
		committerpb.Status_MALFORMED_MISSING_TX_ID,
		committerpb.Status_REJECTED_DUPLICATE_TX_ID:
		return false
	default:
		return true
	}
}

func debugTx(ref *committerpb.TxRef, format string, a ...any) {
	if !logger.IsEnabledFor(zapcore.DebugLevel) {
		return
	}
	txID := "<no-id>"
	if ref.TxId != "" {
		txID = ref.TxId
	}
	logger.Debugf("ID [%s]: %s", txID, fmt.Sprintf(format, a...))
}

func configTx(value []byte) *applicationpb.Tx {
	return &applicationpb.Tx{
		Namespaces: []*applicationpb.TxNamespace{{
			NsId:      committerpb.ConfigNamespaceID,
			NsVersion: 0,
			BlindWrites: []*applicationpb.Write{{
				Key:   []byte(committerpb.ConfigKey),
				Value: value,
			}},
		}},
	}
}

// verifyTxForm verifies that a TX is not malformed.
// It returns status MALFORMED_<reason> if it is malformed, or not-validated otherwise.
func verifyTxForm(tx *applicationpb.Tx) committerpb.Status {
	if len(tx.Namespaces) == 0 {
		return committerpb.Status_MALFORMED_EMPTY_NAMESPACES
	}
	if status := checkEndorsements(tx); status != statusNotYetValidated {
		return status
	}

	nsIDs := make(map[string]any, len(tx.Namespaces))
	for _, ns := range tx.Namespaces {
		// Checks that the application does not submit a config TX.
		if ns.NsId == committerpb.ConfigNamespaceID || policy.ValidateNamespaceID(ns.NsId) != nil {
			return committerpb.Status_MALFORMED_NAMESPACE_ID_INVALID
		}
		if _, ok := nsIDs[ns.NsId]; ok {
			return committerpb.Status_MALFORMED_DUPLICATE_NAMESPACE
		}

		for _, check := range []func(ns *applicationpb.TxNamespace) committerpb.Status{
			checkNamespaceFormation, checkMetaNamespace,
		} {
			if status := check(ns); status != statusNotYetValidated {
				return status
			}
		}
		nsIDs[ns.NsId] = nil
	}
	return statusNotYetValidated
}

func checkEndorsements(tx *applicationpb.Tx) committerpb.Status {
	if len(tx.Namespaces) != len(tx.Endorsements) {
		return committerpb.Status_MALFORMED_MISSING_SIGNATURE
	}
	for _, e := range tx.Endorsements {
		if e == nil || len(e.EndorsementsWithIdentity) == 0 {
			return committerpb.Status_MALFORMED_MISSING_SIGNATURE
		}
		for _, ei := range e.EndorsementsWithIdentity {
			if ei == nil || len(ei.Endorsement) == 0 {
				return committerpb.Status_MALFORMED_MISSING_SIGNATURE
			}
		}
		// Note: we do not validate the Identity field here because the sidecar does not know
		// whether the namespace uses an MSP rule or a threshold rule for endorsement.
		// Threshold rules do not require an identity. Identity validation is left to the
		// signature verifier, which has the policy context to determine what is required.
	}
	return statusNotYetValidated
}

func checkNamespaceFormation(ns *applicationpb.TxNamespace) committerpb.Status {
	if len(ns.ReadWrites) == 0 && len(ns.BlindWrites) == 0 {
		return committerpb.Status_MALFORMED_NO_WRITES
	}

	keys := make([][]byte, 0, len(ns.ReadsOnly)+len(ns.ReadWrites)+len(ns.BlindWrites))
	for _, r := range ns.ReadsOnly {
		keys = append(keys, r.Key)
	}
	for _, r := range ns.ReadWrites {
		keys = append(keys, r.Key)
	}
	for _, r := range ns.BlindWrites {
		keys = append(keys, r.Key)
	}
	return checkKeys(keys)
}

func checkMetaNamespace(txNs *applicationpb.TxNamespace) committerpb.Status {
	if txNs.NsId != committerpb.MetaNamespaceID {
		return statusNotYetValidated
	}
	if len(txNs.BlindWrites) > 0 {
		return committerpb.Status_MALFORMED_BLIND_WRITES_NOT_ALLOWED
	}

	nsUpdate := make(map[string]any)
	u := policy.GetUpdatesFromNamespace(txNs)
	if u == nil {
		return statusNotYetValidated
	}
	for _, pd := range u.NamespacePolicies.Policies {
		// The identity deserializer is not needed because it is only
		// used when evaluating signatures. Since this policy is created
		// only to validate its form, we can skip the deserializer.
		_, err := policy.CreateNamespaceVerifier(pd, nil)
		if err != nil {
			if errors.Is(err, policy.ErrInvalidNamespaceID) {
				return committerpb.Status_MALFORMED_NAMESPACE_ID_INVALID
			}
			return committerpb.Status_MALFORMED_NAMESPACE_POLICY_INVALID
		}
		if pd.Namespace == committerpb.MetaNamespaceID {
			return committerpb.Status_MALFORMED_NAMESPACE_POLICY_INVALID
		}
		if _, ok := nsUpdate[pd.Namespace]; ok {
			return committerpb.Status_MALFORMED_NAMESPACE_POLICY_INVALID
		}
		nsUpdate[pd.Namespace] = nil
	}
	return statusNotYetValidated
}

// checkKeys verifies there are no duplicate keys and no nil keys.
func checkKeys(keys [][]byte) committerpb.Status {
	uniqueKeys := make(map[string]any, len(keys))
	for _, k := range keys {
		if len(k) == 0 {
			return committerpb.Status_MALFORMED_EMPTY_KEY
		}
		uniqueKeys[string(k)] = nil
	}
	if len(uniqueKeys) != len(keys) {
		return committerpb.Status_MALFORMED_DUPLICATE_KEY_IN_READ_WRITE_SET
	}
	return statusNotYetValidated
}
