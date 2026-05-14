/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package utils

import (
	"fmt"
	"testing"

	"github.com/golang/protobuf/proto" //nolint:staticcheck // proto.EncodeVarint does not exist in the new version
	"github.com/stretchr/testify/require"
)

func TestBasicEncodingDecoding(t *testing.T) {
	t.Parallel()
	for i := range uint64(10_000) {
		value := EncodeOrderPreservingVarUint64(i)
		nextValue := EncodeOrderPreservingVarUint64(i + 1)
		require.Greaterf(
			t, nextValue, value,
			"A smaller integer should result in smaller bytes. [%d]=>[%x], [%d]=>[%x]",
			i, i+1, value, nextValue,
		)
		decodedValue, _, err := DecodeOrderPreservingVarUint64(value)
		require.NoError(t, err, "Error via calling DecodeOrderPreservingVarUint64")
		require.Equal(t, i, decodedValue, "Value not same after decoding")
	}
}

func TestDecodingAppendedValues(t *testing.T) {
	t.Parallel()
	var appendedValues []byte
	for i := range uint64(1_000) {
		appendedValues = append(appendedValues, EncodeOrderPreservingVarUint64(i)...)
	}
	for i := range uint64(1_000) {
		value, sz, err := DecodeOrderPreservingVarUint64(appendedValues)
		require.NoError(t, err, "Error via calling DecodeOrderPreservingVarUint64")
		require.Equal(t, i, value, "Value not same after decoding")
		appendedValues = appendedValues[sz:]
	}
}

func TestDecodingBadInputBytes(t *testing.T) {
	t.Parallel()
	// error case when num consumed bytes > 1
	sizeBytes := proto.EncodeVarint(uint64(1000))
	_, _, err := DecodeOrderPreservingVarUint64(sizeBytes)
	require.Error(t, err)
	require.Equal(
		t,
		fmt.Sprintf(
			"number of consumed bytes from DecodeVarint is invalid, expected 1, but got %d",
			len(sizeBytes),
		), err.Error(),
	)

	// error case when decoding invalid bytes - trim off last byte
	invalidSizeBytes := sizeBytes[0 : len(sizeBytes)-1]
	_, _, err = DecodeOrderPreservingVarUint64(invalidSizeBytes)
	require.Error(t, err)
	require.Equal(
		t,
		"number of consumed bytes from DecodeVarint is invalid, expected 1, but got 0",
		err.Error(),
	)

	// error case when size is more than available bytes
	inputBytes := proto.EncodeVarint(uint64(8))
	_, _, err = DecodeOrderPreservingVarUint64(inputBytes)
	require.Error(t, err)
	require.Equal(t, "decoded size (8) from DecodeVarint is more than available bytes (0)", err.Error())

	// error case when size is greater than 8
	bigSizeBytes := proto.EncodeVarint(uint64(12))
	_, _, err = DecodeOrderPreservingVarUint64(bigSizeBytes)
	require.Error(t, err)
	require.Equal(t, "decoded size from DecodeVarint is invalid, expected <=8, but got 12", err.Error())
}
