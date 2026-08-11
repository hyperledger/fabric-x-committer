/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import "github.com/stretchr/testify/require"

// TestingT is the assertion surface shared by [testing.T] and [assert.CollectT]: what
// [require.TestingT] carries, plus Helper. A helper that takes it can be called both from a test and
// from inside an [require.Eventually] condition — which runs on its own goroutine and may still be
// running after the test completed, where asserting on the test's own T panics the whole test binary
// rather than failing one test.
//
// Prefer this over a bare [require.TestingT], which cannot call Helper and so reports failures against
// the helper's own line instead of the caller's. [testing.TB] cannot serve either: it is sealed by an
// unexported method, so an [assert.CollectT] can never implement it. Use [testing.TB] only for a
// helper that needs the rest of it (Cleanup, TempDir, Context, ...) and is never called from a
// condition.
type TestingT interface {
	require.TestingT
	Helper()
}
