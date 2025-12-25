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

// AreSameHeight returns true if both references' heights are either nil or equal.
func AreSameHeight(h1, h2 *TxRef) bool {
	if h1 == nil {
		return h2 == nil
	}
	if h2 == nil {
		return false
	}
	return h1.IsHeight(h2.BlockNum, h2.TxNum)
}

// IsHeight returns true if the reference heights matches the input.
func (x *TxRef) IsHeight(blockNum uint64, txNum uint32) bool {
	return x.BlockNum == blockNum && x.TxNum == txNum
}
