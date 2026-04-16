/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package serve

import (
	"net"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

// PreAllocateListener allocates a port and binds ahead of server initialization.
// It stores the listener object internally for reuse by later listener calls.
func PreAllocateListener(tb testing.TB, c *ServerConfig) net.Listener {
	tb.Helper()
	if c.preAllocatedListener != nil {
		return c.preAllocatedListener
	}
	listener, err := c.Listener(tb.Context())
	require.NoError(tb, err)
	c.preAllocatedListener = listener
	tb.Cleanup(func() {
		connection.CloseConnectionsLog(listener)
	})
	return listener
}

// ClosePreAllocatedListener closes the pre-allocated listener if it exists.
func ClosePreAllocatedListener(c *ServerConfig) {
	if c.preAllocatedListener == nil {
		return
	}
	listener := c.preAllocatedListener
	c.preAllocatedListener = nil
	connection.CloseConnectionsLog(listener)
}
