/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package sidecar

import (
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTxParsingWorkerCount(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		config   TxParsingConfig
		txCount  int
		expected int
	}{
		{name: "empty block", config: TxParsingConfig{MinBatchSize: 256}},
		{name: "small block", config: TxParsingConfig{MinBatchSize: 256}, txCount: 511, expected: 1},
		{
			name: "automatic CPU limit", config: TxParsingConfig{MinBatchSize: 256},
			txCount: 256 * runtime.GOMAXPROCS(0), expected: runtime.GOMAXPROCS(0),
		},
		{
			name: "automatic batch limit", config: TxParsingConfig{MinBatchSize: 256},
			txCount: 768, expected: min(runtime.GOMAXPROCS(0), 3),
		},
		{
			name: "explicit serial", config: TxParsingConfig{MaxWorkers: 1, MinBatchSize: 256},
			txCount: 10000, expected: 1,
		},
		{
			name: "explicit worker limit", config: TxParsingConfig{MaxWorkers: 4, MinBatchSize: 256},
			txCount: 10000, expected: 4,
		},
		{
			name: "explicit batch limit", config: TxParsingConfig{MaxWorkers: 8, MinBatchSize: 256},
			txCount: 1000, expected: 3,
		},
		{
			name: "zero batch size", config: TxParsingConfig{MaxWorkers: 4},
			txCount: 3, expected: 3,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.expected, tc.config.workerCount(tc.txCount))
		})
	}
}
