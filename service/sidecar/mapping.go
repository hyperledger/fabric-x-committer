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
	"github.com/hyperledger/fabric-x-committer/utils/serialization"
)

type (
	validationCode    = byte
	scBlockWithStatus struct {
		block      *protocoordinatorservice.Block
		withStatus *blockWithStatus
		isConfig   bool
	}
)

const (
	statusNotYetValidated = protoblocktx.Status_NOT_VALIDATED
)

func mapBlock(block *common.Block) *scBlockWithStatus {
	if block.Data == nil {
		logger.Warnf("Received a block [%d] without data", block.Header.Number)
		return &scBlockWithStatus{
			block: &protocoordinatorservice.Block{
				Number: block.Header.Number,
			},
			withStatus: &blockWithStatus{
				block:       block,
				txIDToTxNum: make(map[string]int),
			},
		}
	}
	txCount := len(block.Data.Data)
	mappedBlock := &scBlockWithStatus{
		block: &protocoordinatorservice.Block{
			Number: block.Header.Number,
			Txs:    make([]*protoblocktx.Tx, 0, txCount),
			TxsNum: make([]uint32, 0, txCount),
		},
		withStatus: &blockWithStatus{
			block:        block,
			txStatus:     make([]validationCode, txCount),
			txIDToTxNum:  make(map[string]int, txCount),
			pendingCount: txCount,
		},
	}

	for txNum, msg := range block.Data.Data {
		logger.Debugf("Mapping transaction [blk,tx] = [%d,%d]", block.Header.Number, txNum)
		data, hdr, envErr := serialization.UnwrapEnvelope(msg)
		if envErr != nil {
			mappedBlock.excludeTx(protoblocktx.Status_MALFORMED_UNSUPPORTED_TX_PAYLOAD, txNum, hdr, envErr.Error())
			continue
		}
		if hdr.TxId == "" {
			mappedBlock.excludeTx(protoblocktx.Status_MALFORMED_MISSING_TX_ID, txNum, hdr, "no header TX ID")
			continue
		}

		switch common.HeaderType(hdr.Type) {
		case common.HeaderType_CONFIG:
			_, err := policy.ParsePolicyFromConfigTx(msg)
			if err != nil {
				mappedBlock.excludeTx(protoblocktx.Status_MALFORMED_CONFIG_TX_INVALID, txNum, hdr, err.Error())
				continue
			}
			mappedBlock.appendTx(txNum, hdr, configTx(hdr.TxId, msg))
			mappedBlock.isConfig = true
		case common.HeaderType_MESSAGE:
			tx, err := serialization.UnmarshalTx(data)
			if err != nil {
				mappedBlock.excludeTx(protoblocktx.Status_MALFORMED_UNSUPPORTED_TX_PAYLOAD, txNum, hdr, err.Error())
				continue
			}
			if status := verifyTxForm(tx); status != statusNotYetValidated {
				mappedBlock.excludeTx(status, txNum, hdr, "malformed tx")
				continue
			}
			mappedBlock.appendTx(txNum, hdr, tx)
		default:
			mappedBlock.excludeTx(protoblocktx.Status_MALFORMED_UNSUPPORTED_TX_PAYLOAD, txNum, hdr, "message type")
		}
	}
	return mappedBlock
}

func (b *scBlockWithStatus) appendTx(txNum int, channelHdr *common.ChannelHeader, tx *protoblocktx.Tx) {
	_, ok := b.withStatus.txIDToTxNum[tx.Id]
	if ok {
		b.excludeTx(protoblocktx.Status_REJECTED_DUPLICATE_TX_ID, txNum, channelHdr, "duplicate tx")
		return
	}
	b.block.TxsNum = append(b.block.TxsNum, uint32(txNum)) //nolint:gosec
	b.block.Txs = append(b.block.Txs, tx)
	b.withStatus.txIDToTxNum[tx.Id] = txNum
	debugTx(channelHdr, "including [==> %s]", tx.Id)
}

func (b *scBlockWithStatus) excludeTx(
	status protoblocktx.Status, txNum int, channelHdr *common.ChannelHeader, reason string,
) {
	b.withStatus.txStatus[txNum] = validationCode(status)
	b.withStatus.pendingCount--
	debugTx(channelHdr, "excluded: %s (%s)", &status, reason)
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

func configTx(id string, value []byte) *protoblocktx.Tx {
	return &protoblocktx.Tx{
		Id: id,
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
	if tx.Id == "" || !utf8.ValidString(tx.Id) {
		// ASN.1. Marshalling only supports valid UTF8 strings.
		// This case is unlikely as the message received via protobuf message which also only support
		// valid UTF8 strings.
		// Thus, we do not create a designated status for such error.
		return protoblocktx.Status_MALFORMED_MISSING_TX_ID
	}

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
	seenKeys := make(map[string]any, len(keys))
	for _, k := range keys {
		if len(k) == 0 {
			return protoblocktx.Status_MALFORMED_EMPTY_KEY
		}
		seenKeys[string(k)] = nil
	}
	if len(seenKeys) != len(keys) {
		return protoblocktx.Status_MALFORMED_DUPLICATE_KEY_IN_READ_WRITE_SET
	}
	return statusNotYetValidated
}
