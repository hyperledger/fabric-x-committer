/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import "testing"

// ReportTxPerSecond reports a tx/s custom metric on the benchmark, using b.N
// as the number of transactions processed. Benchmarks that batch work (e.g.,
// into blocks) should still size b.N in transactions so that both ns/op and
// tx/s are reported per transaction.
func ReportTxPerSecond(b *testing.B) {
	b.Helper()
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "tx/s")
}
