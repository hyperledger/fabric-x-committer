/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package coordinator

import (
	"testing"

	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/hyperledger/fabric-x-committer/utils/retry"
)

// TestClassifyStreamRecvError covers the retry decision both managers make on a receive error.
// The verifier manager treated InvalidArgument as non-retryable while the validator committer
// manager retried it forever, so the two managers are now bound to the same classification.
func TestClassifyStreamRecvError(t *testing.T) {
	t.Parallel()
	// Non-retryable: the request can never be accepted, so retrying cannot help.
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "invalid argument", err: status.Error(codes.InvalidArgument, "bad policy")},
		{
			name: "invalid argument wrapped",
			err:  errors.Wrap(status.Error(codes.InvalidArgument, "bad policy"), "receive failed"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := classifyStreamRecvError(tc.err)
			require.ErrorIs(t, err, retry.ErrNonRetryable)
		})
	}
	// Retryable: a transient or terminal stream condition that a new stream may recover from.
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "unavailable", err: status.Error(codes.Unavailable, "connection refused")},
		{name: "canceled", err: status.Error(codes.Canceled, "context canceled")},
		{name: "internal", err: status.Error(codes.Internal, "boom")},
		{name: "plain error", err: errors.New("EOF")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := classifyStreamRecvError(tc.err)
			require.NotErrorIs(t, err, retry.ErrNonRetryable)
			require.ErrorContains(t, err, "receive from stream ended with error")
		})
	}
}
