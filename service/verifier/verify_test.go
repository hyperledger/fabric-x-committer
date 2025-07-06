/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package verifier

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/utils/logging"
)

func TestVerifyBadTxForm(t *testing.T) {
	t.Parallel()
	for _, tt := range BadTxFormatTestCases {
		tx := tt.Tx
		expectedStatus := tt.ExpectedStatus
		if tt.ExpectedStatus == protoblocktx.Status_ABORTED_SIGNATURE_INVALID {
			// verifyTxForm doesn't check for signature.
			expectedStatus = protoblocktx.Status_COMMITTED
		}
		t.Run(tt.Tx.Id, func(t *testing.T) {
			t.Parallel()
			actualStatus := verifyTxForm(tx)
			require.Equal(t, expectedStatus, actualStatus)
		})
	}
}

func BenchmarkVerifyForm(b *testing.B) {
	logging.SetupWithConfig(&logging.Config{Enabled: false})
	txs := workload.GenerateTransactions(b, workload.DefaultProfile(8), b.N)

	var status protoblocktx.Status
	b.ResetTimer()
	for _, tx := range txs {
		status = verifyTxForm(tx)
	}
	b.StopTimer()
	require.NotEqual(b, protoblocktx.Status_NOT_VALIDATED, status)
}
