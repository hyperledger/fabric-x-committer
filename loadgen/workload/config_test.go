/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package workload

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTransactionProfileValidate(t *testing.T) {
	t.Parallel()

	// key-backref-rate 0 (default) is the historical fresh-key workload; tx-reference-gap and
	// key-lookback-window are optional and never cause a failure, whatever their value.
	require.NoError(t, (&TransactionProfile{}).Validate())
	require.NoError(t, (&TransactionProfile{ReadWriteCount: 2}).Validate())
	require.NoError(t, (&TransactionProfile{ReadOnlyCount: 2, TxReferenceGap: 5}).Validate())

	// References enabled: the rate is bounded by the total slot count. A zero or small lookback window is
	// always accepted (references step back beyond it as needed).
	// (Non-negativity of key-backref-rate is enforced by the `validate:"gte=0"` struct tag, not here.)
	valid := &TransactionProfile{
		ReadOnlyCount: 2, ReadWriteCount: 2, BlindWriteCount: 1,
		KeyBackrefRate: 2.5, TxReferenceGap: 10, KeyLookbackWindow: 3,
	}
	require.NoError(t, valid.Validate()) // 2.5 <= totalSlots=5

	// A window far smaller than the slot count (even 0) is valid.
	smallWindow := *valid
	smallWindow.KeyLookbackWindow = 0
	require.NoError(t, smallWindow.Validate())

	// key-backref-rate up to the TOTAL slot count is valid (every slot becomes a backward reference).
	upToTotal := *valid
	upToTotal.KeyBackrefRate = 5 // == totalSlots
	require.NoError(t, upToTotal.Validate())

	// key-backref-rate above the total slot count is rejected.
	tooMany := *valid
	tooMany.KeyBackrefRate = 6 // > totalSlots=5
	require.Error(t, tooMany.Validate())
}
