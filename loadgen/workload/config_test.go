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

// TestTransactionProfileValidateQueriesRate covers queries-rate's cross-field check: it must not exceed
// the versioned-read count (read-only + read-write; blind writes carry no version), and this bound is
// enforced regardless of key-backref-rate (queries-rate is independent of the split).
func TestTransactionProfileValidateQueriesRate(t *testing.T) {
	t.Parallel()
	// 2 read-only + 2 read-write = 4 versioned reads; 3 write-count (blind) never counts toward the bound.
	base := TransactionProfile{ReadOnlyCount: 2, ReadWriteCount: 2, BlindWriteCount: 3}

	for _, tc := range []struct {
		name    string
		profile TransactionProfile
	}{
		{name: "queries-rate below the versioned-read count", profile: withQueriesRate(base, 1)},
		{name: "queries-rate at the versioned-read count", profile: withQueriesRate(base, 4)},
		{name: "queries-rate zero (default, disabled)", profile: withQueriesRate(base, 0)},
		{
			name:    "queries-rate at the versioned-read count with key-backref-rate also set",
			profile: withKeyBackrefRate(withQueriesRate(base, 4), 2),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.NoError(t, tc.profile.Validate())
		})
	}

	for _, tc := range []struct {
		name    string
		profile TransactionProfile
	}{
		{name: "queries-rate above the versioned-read count", profile: withQueriesRate(base, 4.5)},
		{
			name:    "queries-rate above the versioned-read count, independent of key-backref-rate being zero",
			profile: withQueriesRate(TransactionProfile{ReadOnlyCount: 1, ReadWriteCount: 1}, 3),
		},
		{
			name:    "queries-rate above the versioned-read count even with key-backref-rate also valid",
			profile: withKeyBackrefRate(withQueriesRate(base, 5), 2),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Error(t, tc.profile.Validate())
		})
	}
}

// withQueriesRate returns a copy of p with QueriesRate set.
func withQueriesRate(p TransactionProfile, rate float64) TransactionProfile {
	p.QueriesRate = rate
	return p
}

// withKeyBackrefRate returns a copy of p with backward references enabled at rate.
func withKeyBackrefRate(p TransactionProfile, rate float64) TransactionProfile {
	p.KeyBackrefRate = rate
	return p
}
