/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package committerpb

const (
	// MetaNamespaceID is the system's namespace ID that holds information about application's namespaces.
	MetaNamespaceID = "_meta"
	// ConfigNamespaceID is the system's namespace ID that holds the config transaction.
	ConfigNamespaceID = "_config"
	// ConfigKey is the key of the config transaction.
	ConfigKey = "_config"
)

// ComparableTxRef is used as a comparable version of [TxRef].
// It can be used as a map key.
type ComparableTxRef struct {
	TxID     string
	BlockNum uint64
	TxNum    uint32
}

// NewTxRef is a convenient method to create a full TX reference in a single line.
func NewTxRef(txID string, blockNum uint64, txNum uint32) *TxRef {
	return &TxRef{TxId: txID, BlockNum: blockNum, TxNum: txNum}
}

// NewTxStatus is a convenient method to create a [TxStatus] in a single line.
func NewTxStatus(s Status, txID string, blkNum uint64, txNum uint32) *TxStatus {
	return NewTxStatusFromRef(NewTxRef(txID, blkNum, txNum), s)
}

// NewTxStatusFromRef is a convenient method to create a [TxStatus] from a [TxRef] in a single line.
func NewTxStatusFromRef(ref *TxRef, s Status) *TxStatus {
	return &TxStatus{Ref: ref, Status: s}
}

// ToComparable converts proto TxRef to a ComparableTxRef.
func (ref *TxRef) ToComparable() ComparableTxRef {
	return ComparableTxRef{
		TxID:     ref.TxId,
		BlockNum: ref.BlockNum,
		TxNum:    ref.TxNum,
	}
}

// ToProto converts a ComparableTxRef to a proto TxRef.
func (ref *ComparableTxRef) ToProto() *TxRef {
	return &TxRef{
		TxId:     ref.TxID,
		BlockNum: ref.BlockNum,
		TxNum:    ref.TxNum,
	}
}
