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

func TestNewHeightFromBytes_ErrorCases(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		input       []byte
		expectedErr string
	}{
		{
			name:        "empty bytes",
			input:       []byte{},
			expectedErr: "failed to decode block number from bytes",
		},
		{
			name:        "invalid block number encoding",
			input:       []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF},
			expectedErr: "failed to decode block number from bytes",
		},
		{
			name:        "missing tx number",
			input:       []byte{0x01, 0x05}, // valid block number (size=1, value=5) but no tx number
			expectedErr: "failed to decode tx number from bytes",
		},
		{
			name:        "invalid tx number encoding",
			input:       append([]byte{0x01}, []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}...),
			expectedErr: "failed to decode tx number from bytes",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, n, err := NewHeightFromBytes(tc.input)
			require.ErrorContains(t, err, tc.expectedErr)
			require.Nil(t, h)
			require.Equal(t, -1, n)
		})
	}
}

func TestHeight_String(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		height   *Height
		expected string
	}{
		{
			name:     "normal height",
			height:   NewHeight(10, 100),
			expected: "{BlockNum: 10, TxNum: 100}",
		},
		{
			name:     "zero values",
			height:   NewHeight(0, 0),
			expected: "{BlockNum: 0, TxNum: 0}",
		},
		{
			name:     "large values",
			height:   NewHeight(18446744073709551615, 4294967295),
			expected: "{BlockNum: 18446744073709551615, TxNum: 4294967295}",
		},
		{
			name:     "nil height",
			height:   nil,
			expected: "<nil>",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			result := tc.height.String()
			require.Equal(t, tc.expected, result)
		})
	}
}
