/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package sidecar

import (
	"fmt"
	"unicode/utf8"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"go.uber.org/zap/zapcore"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
	"github.com/hyperledger/fabric-x-committer/api/protocoordinatorservice"
	"github.com/hyperledger/fabric-x-committer/api/types"
	"github.com/hyperledger/fabric-x-committer/service/verifier/policy"
	"github.com/hyperledger/fabric-x-committer/utils"
	"github.com/hyperledger/fabric-x-committer/utils/serialization"
)

type (
	blockMappingResult struct {
		blockNumber uint64
		block       *protocoordinatorservice.Batch
		withStatus  *blockWithStatus
		isConfig    bool
		// txIDToHeight is a reference to the relay map.
		txIDToHeight *utils.SyncMap[string, types.Height]
	}

	blockWithStatus struct {
		block        *common.Block
		txStatus     []protoblocktx.Status
		pendingCount int
	}
)

const (
	statusNotYetValidated = protoblocktx.Status_NOT_VALIDATED
	statusIdx             = int(common.BlockMetadataIndex_TRANSACTIONS_FILTER)
)

func mapBlock(block *common.Block, txIDToHeight *utils.SyncMap[string, types.Height]) (*blockMappingResult, error) {
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
			blockNumber:  blockNumber,
			block:        &protocoordinatorservice.Batch{},
			withStatus:   &blockWithStatus{block: block},
			txIDToHeight: txIDToHeight,
		}, nil
	}

	txCount := len(block.Data.Data)
	mappedBlock := &blockMappingResult{
		blockNumber: blockNumber,
		block: &protocoordinatorservice.Batch{
			Txs:      make([]*protocoordinatorservice.Tx, 0, txCount),
			Rejected: make([]*protocoordinatorservice.TxStatusInfo, 0, txCount),
		},
		withStatus: &blockWithStatus{
			block:        block,
			txStatus:     make([]protoblocktx.Status, txCount),
			pendingCount: txCount,
		},
		txIDToHeight: txIDToHeight,
	}
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
	data, hdr, envErr := serialization.UnwrapEnvelope(msg)
	if envErr != nil {
		return b.rejectNonDBStatusTx(msgIndex, hdr, protoblocktx.Status_MALFORMED_BAD_ENVELOPE, envErr.Error())
	}
	if hdr.TxId == "" || !utf8.ValidString(hdr.TxId) {
		return b.rejectNonDBStatusTx(msgIndex, hdr, protoblocktx.Status_MALFORMED_MISSING_TX_ID, "no TX ID")
	}

	switch common.HeaderType(hdr.Type) {
	default:
		return b.rejectTx(msgIndex, hdr, protoblocktx.Status_MALFORMED_UNSUPPORTED_ENVELOPE_PAYLOAD, "message type")
	case common.HeaderType_CONFIG:
		_, err := policy.ParsePolicyFromConfigTx(msg)
		if err != nil {
			return b.rejectTx(msgIndex, hdr, protoblocktx.Status_MALFORMED_CONFIG_TX_INVALID, err.Error())
		}
		b.isConfig = true
		return b.appendTx(msgIndex, hdr, configTx(msg))
	case common.HeaderType_MESSAGE:
		tx, err := serialization.UnmarshalTx(data)
		if err != nil {
			return b.rejectTx(msgIndex, hdr, protoblocktx.Status_MALFORMED_BAD_ENVELOPE_PAYLOAD, err.Error())
		}
		if status := verifyTxForm(tx); status != statusNotYetValidated {
			return b.rejectTx(msgIndex, hdr, status, "malformed tx")
		}
		return b.appendTx(msgIndex, hdr, tx)
	}
}

func (b *blockMappingResult) appendTx(txNum uint32, hdr *common.ChannelHeader, tx *protoblocktx.Tx) error {
	if idAlreadyExists, err := b.addTxIDMapping(txNum, hdr); idAlreadyExists || err != nil {
		return err
	}
	b.block.Txs = append(b.block.Txs, &protocoordinatorservice.Tx{
		Ref:     types.TxRef(hdr.TxId, b.blockNumber, txNum),
		Content: tx,
	})
	debugTx(hdr, "included: %s", hdr.TxId)
	return nil
}

func (b *blockMappingResult) rejectTx(
	txNum uint32, hdr *common.ChannelHeader, status protoblocktx.Status, reason string,
) error {
	if !IsStatusStoredInDB(status) {
		return b.rejectNonDBStatusTx(txNum, hdr, status, reason)
	}
	if idAlreadyExists, err := b.addTxIDMapping(txNum, hdr); idAlreadyExists || err != nil {
		return err
	}
	b.block.Rejected = append(b.block.Rejected, &protocoordinatorservice.TxStatusInfo{
		Ref:    types.TxRef(hdr.TxId, b.blockNumber, txNum),
		Status: status,
	})
	debugTx(hdr, "rejected: %s (%s)", &status, reason)
	return nil
}

// rejectNonDBStatusTx is used to reject with statuses that are not stored in the state DB.
// Namely, statuses for cases where we don't have a TX ID, or there is a TX ID duplication.
// For such cases, no notification will be given by the notification service.
func (b *blockMappingResult) rejectNonDBStatusTx(
	txNum uint32, hdr *common.ChannelHeader, status protoblocktx.Status, reason string,
) error {
	if IsStatusStoredInDB(status) {
		// This can never occur unless there is a bug in the relay.
		return errors.Newf("[BUG] status should be stored [blk:%d,num:%d]: %s", b.blockNumber, txNum, &status)
	}
	err := b.withStatus.setFinalStatus(txNum, status)
	if err != nil {
		return err
	}
	debugTx(hdr, "excluded: %s (%s)", &status, reason)
	return nil
}

func (b *blockMappingResult) addTxIDMapping(txNum uint32, hdr *common.ChannelHeader) (idAlreadyExists bool, err error) {
	_, idAlreadyExists = b.txIDToHeight.LoadOrStore(hdr.TxId, types.Height{
		BlockNum: b.blockNumber,
		TxNum:    txNum,
	})
	if idAlreadyExists {
		err = b.rejectNonDBStatusTx(txNum, hdr, protoblocktx.Status_REJECTED_DUPLICATE_TX_ID, "duplicate tx")
	}
	return idAlreadyExists, err
}

func (b *blockWithStatus) setFinalStatus(txNum uint32, status protoblocktx.Status) error {
	if b.txStatus[txNum] != statusNotYetValidated {
		// This can never occur unless there is a bug in the relay or the coordinator.
		return errors.Newf("two results for a TX [blockNum: %d, txNum: %d]", b.block.Header.Number, txNum)
	}
	b.txStatus[txNum] = status
	b.pendingCount--
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
func IsStatusStoredInDB(status protoblocktx.Status) bool {
	switch status {
	case protoblocktx.Status_MALFORMED_BAD_ENVELOPE,
		protoblocktx.Status_MALFORMED_MISSING_TX_ID,
		protoblocktx.Status_REJECTED_DUPLICATE_TX_ID:
		return false
	default:
		return true
	}
}

func debugTx(channelHdr *common.ChannelHeader, format string, a ...any) {
	if logger.Level() > zapcore.DebugLevel {
		return
	}
	hdr := "<no-header>"
	txID := "<no-id>"
	if channelHdr != nil {
		hdr = common.HeaderType(channelHdr.Type).String()
		txID = channelHdr.TxId
	}
	logger.Debugf("TX type [%s] ID [%s]: %s", hdr, txID, fmt.Sprintf(format, a...))
}

func configTx(value []byte) *protoblocktx.Tx {
	return &protoblocktx.Tx{
		Namespaces: []*protoblocktx.TxNamespace{{
			NsId:      types.ConfigNamespaceID,
			NsVersion: 0,
			BlindWrites: []*protoblocktx.Write{{
				Key:   []byte(types.ConfigKey),
				Value: value,
			}},
		}},
	}
}

// verifyTxForm verifies that a TX is not malformed.
// It returns status MALFORMED_<reason> if it is malformed, or not-validated otherwise.
func verifyTxForm(tx *protoblocktx.Tx) protoblocktx.Status {
	if len(tx.Namespaces) == 0 {
		return protoblocktx.Status_MALFORMED_EMPTY_NAMESPACES
	}
	if len(tx.Namespaces) != len(tx.Signatures) {
		return protoblocktx.Status_MALFORMED_MISSING_SIGNATURE
	}

	nsIDs := make(map[string]any, len(tx.Namespaces))
	for _, ns := range tx.Namespaces {
		// Checks that the application does not submit a config TX.
		if ns.NsId == types.ConfigNamespaceID || policy.ValidateNamespaceID(ns.NsId) != nil {
			return protoblocktx.Status_MALFORMED_NAMESPACE_ID_INVALID
		}
		if _, ok := nsIDs[ns.NsId]; ok {
			return protoblocktx.Status_MALFORMED_DUPLICATE_NAMESPACE
		}

		for _, check := range []func(ns *protoblocktx.TxNamespace) protoblocktx.Status{
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

func checkNamespaceFormation(ns *protoblocktx.TxNamespace) protoblocktx.Status {
	if len(ns.ReadWrites) == 0 && len(ns.BlindWrites) == 0 {
		return protoblocktx.Status_MALFORMED_NO_WRITES
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

func checkMetaNamespace(txNs *protoblocktx.TxNamespace) protoblocktx.Status {
	if txNs.NsId != types.MetaNamespaceID {
		return statusNotYetValidated
	}
	if len(txNs.BlindWrites) > 0 {
		return protoblocktx.Status_MALFORMED_BLIND_WRITES_NOT_ALLOWED
	}

	nsUpdate := make(map[string]any)
	u := policy.GetUpdatesFromNamespace(txNs)
	if u == nil {
		return statusNotYetValidated
	}
	for _, pd := range u.NamespacePolicies.Policies {
		_, err := policy.ParseNamespacePolicyItem(pd)
		if err != nil {
			if errors.Is(err, policy.ErrInvalidNamespaceID) {
				return protoblocktx.Status_MALFORMED_NAMESPACE_ID_INVALID
			}
			return protoblocktx.Status_MALFORMED_NAMESPACE_POLICY_INVALID
		}
		if pd.Namespace == types.MetaNamespaceID {
			return protoblocktx.Status_MALFORMED_NAMESPACE_POLICY_INVALID
		}
		if _, ok := nsUpdate[pd.Namespace]; ok {
			return protoblocktx.Status_MALFORMED_NAMESPACE_POLICY_INVALID
		}
		nsUpdate[pd.Namespace] = nil
	}
	return statusNotYetValidated
}

// checkKeys verifies there are no duplicate keys and no nil keys.
func checkKeys(keys [][]byte) protoblocktx.Status {
	uniqueKeys := make(map[string]any, len(keys))
	for _, k := range keys {
		if len(k) == 0 {
			return protoblocktx.Status_MALFORMED_EMPTY_KEY
		}
		uniqueKeys[string(k)] = nil
	}
	if len(uniqueKeys) != len(keys) {
		return protoblocktx.Status_MALFORMED_DUPLICATE_KEY_IN_READ_WRITE_SET
	}
	return statusNotYetValidated
}
