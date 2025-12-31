/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	"github.com/hyperledger/fabric-x-committer/mock"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

func TestCheckServerStopped(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)

	mockSigVer := mock.NewMockSigVerifier()
	sigVerServers := test.StartGrpcServersForTest(ctx, t, 1, func(server *grpc.Server, _ int) {
		mockSigVer.RegisterService(server)
	}, test.InsecureTLSConfig)

	addr := sigVerServers.Configs[0].Endpoint.Address()
	require.Eventually(t, func() bool {
		return !test.CheckServerStopped(t, addr)
	}, 3*time.Second, 250*time.Millisecond)

	sigVerServers.Servers[0].Stop()

	require.Eventually(t, func() bool {
		return test.CheckServerStopped(t, addr)
	}, 3*time.Second, 250*time.Millisecond)
}
