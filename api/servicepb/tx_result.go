/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package servicepb

import "github.com/hyperledger/fabric-x-common/api/committerpb"

// NewTxResults wraps status-only responses without adding version metadata.
func NewTxResults(statuses []*committerpb.TxStatus) []*TxResult {
	if statuses == nil {
		return nil
	}
	results := make([]*TxResult, len(statuses))
	for i, status := range statuses {
		results[i] = &TxResult{Status: status}
	}
	return results
}

// GetStatuses projects results onto the unchanged public status type.
func (x *TxStatusBatch) GetStatuses() []*committerpb.TxStatus {
	if x == nil || x.TxResult == nil {
		return nil
	}
	statuses := make([]*committerpb.TxStatus, len(x.TxResult))
	for i, result := range x.TxResult {
		statuses[i] = result.GetStatus()
	}
	return statuses
}
