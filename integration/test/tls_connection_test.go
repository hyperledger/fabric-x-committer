/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"testing"
	"time"

	"github.com/onsi/gomega"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
	"github.com/hyperledger/fabric-x-committer/integration/runner"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

func TestMutualTLSConnection(t *testing.T) {
	t.Parallel()
	gomega.RegisterTestingT(t)
	c := runner.NewRuntime(t, &runner.Config{
		NumVerifiers: 2,
		NumVCService: 2,
		BlockTimeout: 2 * time.Second,
		BlockSize:    500,
		TLS:          connection.MutualTLSMode,
	})

	c.Start(t, runner.FullTxPathWithLoadGen)

	require.Eventually(t, func() bool {
		count := c.CountStatus(t, protoblocktx.Status_COMMITTED)
		t.Logf("count %d", count)
		return count > 1_000
	}, 90*time.Second, 500*time.Millisecond)
	require.Zero(t, c.CountAlternateStatus(t, protoblocktx.Status_COMMITTED))
}

func TestOneSidedTLSConnection(t *testing.T) {
	t.Parallel()
	gomega.RegisterTestingT(t)
	c := runner.NewRuntime(t, &runner.Config{
		NumVerifiers: 2,
		NumVCService: 2,
		BlockTimeout: 2 * time.Second,
		BlockSize:    500,
		TLS:          connection.ServerSideTLSMode,
	})

	c.Start(t, runner.FullTxPathWithLoadGen)

	require.Eventually(t, func() bool {
		count := c.CountStatus(t, protoblocktx.Status_COMMITTED)
		t.Logf("count %d", count)
		return count > 1_000
	}, 90*time.Second, 500*time.Millisecond)
	require.Zero(t, c.CountAlternateStatus(t, protoblocktx.Status_COMMITTED))
}
