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

// NewTxRef a convenient method to create a full, coordinator's TX reference in a single line.
func NewTxRef(txID string, blockNum uint64, txNum uint32) *TxRef {
	return &TxRef{TxId: txID, BlockNum: blockNum, TxNum: txNum}
}
