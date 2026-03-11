/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package ordererconn_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/utils/ordererconn"
)

func TestGetFaultToleranceLevel(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		input    string
		expected string
		error    bool
	}{
		// Valid cases - uppercase
		{input: "BFT", expected: "BFT"},
		{input: "CFT", expected: "CFT"},
		// Valid cases - lowercase
		{input: "bft", expected: "BFT"},
		{input: "cft", expected: "CFT"},
		// Valid cases - mixed case
		{input: "BfT", expected: "BFT"},
		{input: "CfT", expected: "CFT"},
		// Unspecified defaults to BFT
		{input: "", expected: "BFT"},
		// Invalid case
		{input: "invalid", error: true},
	} {
		t.Run(tt.input, func(t *testing.T) {
			t.Parallel()
			result, err := ordererconn.GetFaultToleranceLevel(tt.input)
			if tt.error {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, tt.expected, result)
			}
		})
	}
}
