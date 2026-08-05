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

	// Split disabled (new-keys-rate unset): always valid; tx-reference-gap / key-lookback-window are ignored.
	require.NoError(t, (&TransactionProfile{}).Validate())
	require.NoError(t, (&TransactionProfile{ReadWriteCount: 2}).Validate())
	require.NoError(t, (&TransactionProfile{ReadOnlyCount: 2, TxReferenceGap: 5}).Validate())

	// Split enabled: rate bounded by the total slot count; window must cover the total slot count.
	// (Non-negativity of new-keys-rate is enforced by the `validate:"gte=0"` struct tag, not here.)
	valid := &TransactionProfile{
		ReadOnlyCount: 2, ReadWriteCount: 2, BlindWriteCount: 1,
		NewKeysRate: new(float64(2.5)), TxReferenceGap: 10, KeyLookbackWindow: 100,
	}
	require.NoError(t, valid.Validate()) // 2.5 <= W=3 ; window 100 >= 5 slots

	// new-keys-rate 0 (no creates) is valid as long as the window covers the slots.
	zero := *valid
	zero.NewKeysRate = new(float64(0))
	require.NoError(t, zero.Validate())

	// new-keys-rate up to the TOTAL slot count is valid (surplus spills into read-only nonexistent reads).
	upToTotal := *valid
	upToTotal.NewKeysRate = new(float64(3.5)) // <= totalSlots=5
	require.NoError(t, upToTotal.Validate())

	// new-keys-rate above the total slot count is rejected.
	tooMany := *valid
	tooMany.NewKeysRate = new(float64(6)) // > totalSlots=5
	require.Error(t, tooMany.Validate())

	// A window smaller than the total slot count is rejected (references could collide within a tx).
	tooSmallWindow := *valid
	tooSmallWindow.KeyLookbackWindow = 4 // < 5 slots
	require.Error(t, tooSmallWindow.Validate())
}
