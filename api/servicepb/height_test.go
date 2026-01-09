/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package servicepb

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestVersionSerialization(t *testing.T) {
	t.Parallel()
	h1 := NewHeight(10, 100)
	b := h1.ToBytes()
	h2, n, err := NewHeightFromBytes(b)
	require.NoError(t, err)
	require.Equal(t, h1, h2)
	require.Len(t, b, n)
}

func TestVersionExtraBytes(t *testing.T) {
	t.Parallel()
	extraBytes := []byte("junk")
	h1 := NewHeight(10, 100)
	b1 := append(h1.ToBytes(), extraBytes...)
	h2, n, err := NewHeightFromBytes(b1)
	require.NoError(t, err)
	require.Equal(t, h1, h2)
	require.Len(t, h1.ToBytes(), n)
	require.Equal(t, extraBytes, b1[n:])
}
