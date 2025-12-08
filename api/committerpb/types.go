/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package committerpb

import "github.com/hyperledger/fabric-x-committer/api/protocoordinatorservice"

const (
	// MetaNamespaceID is the system's namespace ID that holds information about application's namespaces.
	MetaNamespaceID = "_meta"

	// ConfigNamespaceID is the system's namespace ID that holds the config transaction.
	ConfigNamespaceID = "_config"
	// ConfigKey is the key of the config transaction.
	ConfigKey = "_config"
)

// Version is a convenient method to create a version pointer in a single line.
func Version(version uint64) *uint64 {
	return &version
}

// TxRef a convenient method to create a full, coordinator's TX reference in a single line.
func TxRef(txID string, blockNum uint64, txNum uint32) *protocoordinatorservice.TxRef {
	return &protocoordinatorservice.TxRef{TxId: txID, BlockNum: blockNum, TxNum: txNum}
}
