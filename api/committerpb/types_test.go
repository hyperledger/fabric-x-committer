/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package committerpb

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHeightComparison(t *testing.T) {
	t.Parallel()
	require.True(t, AreSameHeight(NewTxRef("tx1", 10, 100), NewTxRef("tx2", 10, 100)))
	require.False(t, AreSameHeight(NewTxRef("tx1", 10, 100), NewTxRef("tx1", 11, 100)))
	require.False(t, AreSameHeight(NewTxRef("tx1", 10, 100), NewTxRef("tx1", 10, 101)))

	require.True(t, AreSameHeight(nil, nil))
	require.False(t, AreSameHeight(NewTxRef("tx1", 10, 100), nil))
	require.False(t, AreSameHeight(nil, NewTxRef("tx1", 10, 100)))

	require.True(t, NewTxRef("tx1", 10, 100).IsHeight(10, 100))
	require.False(t, NewTxRef("tx1", 10, 100).IsHeight(11, 100))
	require.False(t, NewTxRef("tx1", 10, 100).IsHeight(10, 101))
}
