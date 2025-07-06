/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package sidecar

import (
	"fmt"

	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"go.uber.org/zap/zapcore"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
	"github.com/hyperledger/fabric-x-committer/api/protocoordinatorservice"
	"github.com/hyperledger/fabric-x-committer/api/types"
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
	excludedStatus  = validationCode(protoblocktx.Status_ABORTED_UNSUPPORTED_TX_PAYLOAD)
	notYetValidated = validationCode(protoblocktx.Status_NOT_VALIDATED)
)

func mapBlock(block *common.Block) *scBlockWithStatus {
	if block.Data == nil {
		logger.Warnf("Received a block [%d] without data", block.Header.Number)
		return &scBlockWithStatus{
			block: &protocoordinatorservice.Block{
				Number: block.Header.Number,
			},
			withStatus: &blockWithStatus{
				block:         block,
				txIDToTxIndex: make(map[string]int),
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
			block:         block,
			txStatus:      newValidationCodes(txCount),
			txIDToTxIndex: make(map[string]int, txCount),
			pendingCount:  txCount,
		},
	}

	for txNum, msg := range block.Data.Data {
		logger.Debugf("Mapping transaction [blk,tx] = [%d,%d]", block.Header.Number, txNum)
		data, hdr, err := serialization.UnwrapEnvelope(msg)
		if err != nil {
			mappedBlock.excludeTx(txNum, hdr, err.Error())
			continue
		}

		switch hdr.Type {
		case int32(common.HeaderType_CONFIG):
			mappedBlock.appendTx(txNum, hdr, configTx(hdr.TxId, msg))
			mappedBlock.isConfig = true
		case int32(common.HeaderType_MESSAGE):
			tx, err := serialization.UnmarshalTx(data)
			if err != nil {
				mappedBlock.excludeTx(txNum, hdr, err.Error())
				continue
			}
			if isApplicationConfigTx(tx) {
				mappedBlock.excludeTx(txNum, hdr, "application's config tx")
				continue
			}
			mappedBlock.appendTx(txNum, hdr, tx)
		default:
			mappedBlock.excludeTx(txNum, hdr, "unsupported message type")
		}
	}
	return mappedBlock
}

func (b *scBlockWithStatus) appendTx(txNum int, channelHdr *common.ChannelHeader, tx *protoblocktx.Tx) {
	b.block.TxsNum = append(b.block.TxsNum, uint32(txNum)) //nolint:gosec
	b.block.Txs = append(b.block.Txs, tx)
	debugTx(channelHdr, "including [==> %s]", tx.Id)
}

func (b *scBlockWithStatus) excludeTx(txNum int, channelHdr *common.ChannelHeader, reason string) {
	b.withStatus.txStatus[txNum] = excludedStatus
	b.withStatus.pendingCount--
	debugTx(channelHdr, "excluding due to %s", reason)
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

func newValidationCodes(expected int) []validationCode {
	codes := make([]validationCode, expected)
	for i := range codes {
		codes[i] = notYetValidated
	}
	return codes
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
		// A valid TX must have a signature per namespace.
		Signatures: make([][]byte, 1),
	}
}

// isApplicationConfigTx checks the application does not submit a config TX.
func isApplicationConfigTx(tx *protoblocktx.Tx) bool {
	for _, ns := range tx.Namespaces {
		if ns.NsId == types.ConfigNamespaceID {
			return true
		}
	}
	return false
}
