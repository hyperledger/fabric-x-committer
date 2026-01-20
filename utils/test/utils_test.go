/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/mock"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

func TestCheckServerStopped(t *testing.T) {
	t.Parallel()

	_, sigVerServers := mock.StartMockVerifierService(t, 1)

	addr := sigVerServers.Configs[0].Endpoint.Address()
	require.Eventually(t, func() bool {
		return !test.CheckServerStopped(t, addr)
	}, 3*time.Second, 250*time.Millisecond)

	sigVerServers.Servers[0].Stop()

	require.Eventually(t, func() bool {
		return test.CheckServerStopped(t, addr)
	}, 3*time.Second, 250*time.Millisecond)
}
