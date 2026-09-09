/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package mock

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"google.golang.org/protobuf/types/known/emptypb"
)

// deleteCloneRecorder implements DeleteDBCloneForSnapshot for the mock coordinator and the
// mock vcservice. Both sit on the same forwarding chain (sidecar -> coordinator ->
// vcservice) and a test asserts the same two things of either: which tx_ids arrived, and
// that the downstream gRPC status code is passed through unchanged. It is embedded rather
// than duplicated so the two mocks cannot drift.
type deleteCloneRecorder struct {
	// requests holds the tx_id of every call in arrival order, guarded by mu; err is the
	// error the calls return, so a test can inject a downstream failure.
	requests []string
	mu       sync.Mutex
	err      atomic.Pointer[error]
}

// DeleteDBCloneForSnapshot records the snapshot-clone deletion request so a test can assert
// it was forwarded, and returns the error injected by SetDeleteDBCloneError.
func (r *deleteCloneRecorder) DeleteDBCloneForSnapshot(
	_ context.Context,
	req *committerpb.DeleteDBCloneForSnapshotRequest,
) (*emptypb.Empty, error) {
	r.mu.Lock()
	r.requests = append(r.requests, req.GetTxId())
	r.mu.Unlock()
	if err := r.err.Load(); err != nil {
		return nil, *err
	}
	return &emptypb.Empty{}, nil
}

// DeleteDBCloneRequests returns the tx_ids of every DeleteDBCloneForSnapshot call received,
// in arrival order.
func (r *deleteCloneRecorder) DeleteDBCloneRequests() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.requests)
}

// SetDeleteDBCloneError makes subsequent DeleteDBCloneForSnapshot calls fail with err, so a
// test can assert the caller propagates the downstream gRPC status code unchanged.
func (r *deleteCloneRecorder) SetDeleteDBCloneError(err error) {
	r.err.Store(&err)
}
