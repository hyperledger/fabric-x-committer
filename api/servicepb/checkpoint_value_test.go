/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package servicepb_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
)

func TestCheckpointValue(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		value []byte
		valid bool
	}{
		{name: "abort", value: []byte("abort"), valid: true},
		{name: "nil"},
		{name: "empty", value: []byte{}},
		{name: "short", value: []byte("ab")},
		{name: "case", value: []byte("ABORT")},
		{name: "suffix", value: []byte("abort\x00")},
		{name: "31 bytes", value: make([]byte, 31)},
		{name: "32 bytes", value: make([]byte, 32), valid: true},
		{name: "33 bytes", value: make([]byte, 33)},
		{name: "marker-prefixed digest", value: append([]byte("abort"), make([]byte, 27)...), valid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.valid, servicepb.IsValidCheckpointValue(tc.value))
		})
	}
}
