/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package servicepb

import (
	"fmt"

	"github.com/hyperledger/fabric-x-committer/api/applicationpb"
	"github.com/hyperledger/fabric-x-committer/utils"
)

// Height represents the height of a transaction in blockchain.
type Height struct {
	BlockNum uint64
	TxNum    uint32
}

// NewStatusWithHeight creates a protoblocktx.StatusWithHeight with the given values.
func NewStatusWithHeight(s applicationpb.Status, blkNum uint64, txNum uint32) *applicationpb.StatusWithHeight {
	return &applicationpb.StatusWithHeight{
		Code:        s,
		BlockNumber: blkNum,
		TxNumber:    txNum,
	}
}

// NewStatusWithHeightFromRef creates a protoblocktx.StatusWithHeight with the given values.
func NewStatusWithHeightFromRef(
	s applicationpb.Status, ref *TxRef,
) *applicationpb.StatusWithHeight {
	return NewStatusWithHeight(s, ref.BlockNum, ref.TxNum)
}

// NewHeight constructs a new instance of Height.
func NewHeight(blockNum uint64, txNum uint32) *Height {
	return &Height{BlockNum: blockNum, TxNum: txNum}
}

// NewHeightFromTxRef constructs a new instance of Height.
func NewHeightFromTxRef(ref *TxRef) *Height {
	return NewHeight(ref.BlockNum, ref.TxNum)
}

// NewHeightFromBytes constructs a new instance of Height from serialized bytes.
func NewHeightFromBytes(b []byte) (*Height, int, error) {
	blockNum, n1, err := utils.DecodeOrderPreservingVarUint64(b)
	if err != nil {
		return nil, -1, fmt.Errorf("failed to decode block number from bytes [%v]: %w", b, err)
	}
	txNum, n2, err := utils.DecodeOrderPreservingVarUint64(b[n1:])
	if err != nil {
		return nil, -1, fmt.Errorf("failed to decode tx number from bytes [%v]: %w", b, err)
	}
	return NewHeight(blockNum, uint32(txNum)), n1 + n2, nil //nolint:gosec
}

// WithStatus creates protoblocktx.StatusWithHeight with this height and the given code.
func (h *Height) WithStatus(code applicationpb.Status) *applicationpb.StatusWithHeight {
	return NewStatusWithHeight(code, h.BlockNum, h.TxNum)
}

// ToBytes serializes the Height.
func (h *Height) ToBytes() []byte {
	blockNumBytes := utils.EncodeOrderPreservingVarUint64(h.BlockNum)
	txNumBytes := utils.EncodeOrderPreservingVarUint64(uint64(h.TxNum))
	return append(blockNumBytes, txNumBytes...)
}

// Compare returns -1, zero, or +1 based on whether this height is
// less than, equals to, or greater than the specified height respectively.
func (h *Height) Compare(h1 *Height) int {
	switch {
	case h.BlockNum < h1.BlockNum:
		return -1
	case h.BlockNum > h1.BlockNum:
		return 1
	case h.TxNum < h1.TxNum:
		return -1
	case h.TxNum > h1.TxNum:
		return 1
	default:
		return 0
	}
}

// String returns string for printing.
func (h *Height) String() string {
	if h == nil {
		return "<nil>"
	}
	return fmt.Sprintf("{BlockNum: %d, TxNum: %d}", h.BlockNum, h.TxNum)
}

// AreSame returns true if both the heights are either nil or equal.
func AreSame(h1, h2 *Height) bool {
	if h1 == nil {
		return h2 == nil
	}
	if h2 == nil {
		return false
	}
	return h1.Compare(h2) == 0
}
