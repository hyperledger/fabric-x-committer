/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import "testing"

// ReportTxPerSecond reports a tx/s custom metric on the benchmark.
func ReportTxPerSecond(b *testing.B) {
	b.Helper()
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "tx/s")
}
