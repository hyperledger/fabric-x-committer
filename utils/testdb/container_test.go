/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package testdb

import (
	"sync"
	"testing"
	"time"

	docker "github.com/fsouza/go-dockerclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// GetContainerLogs and ExecuteCommand accept a [test.TestingT] so that a polling condition can
// pass its [assert.CollectT]. That is what stops a probe which is still running after its test
// completed from panicking the whole test binary instead of failing one test. These tests pin the
// runtime half of that contract: a failing probe fails only the tick, the wait keeps retrying, and
// the test's own T is never touched.
func TestContainerProbeFailsTheTickNotTheTest(t *testing.T) {
	t.Parallel()
	dc := newContainerWithBogusID(t)

	for _, tc := range []struct {
		name  string
		probe func(ct *assert.CollectT)
	}{
		{name: "GetContainerLogs", probe: func(ct *assert.CollectT) { dc.GetContainerLogs(ct) }},
		{name: "ExecuteCommand", probe: func(ct *assert.CollectT) { dc.ExecuteCommand(ct, []string{"true"}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// A stand-in for the outer T, so the wait's own verdict is observable without this
			// test failing on it.
			var outer errorRecorder
			satisfied := assert.EventuallyWithT(&outer, tc.probe, 300*time.Millisecond, 50*time.Millisecond)

			require.False(t, satisfied, "a failing probe must not satisfy the condition")
			require.NotZero(t, outer.count(), "the wait must report the failure")
		})
	}
}

// StopAndRemoveContainer must tolerate a node whose container was never created, so that a cluster
// teardown registered before startup can still run after a partial start.
func TestStopAndRemoveContainerWithoutContainerIsNoOp(t *testing.T) {
	t.Parallel()
	dc := &DatabaseContainer{Name: "sc_test_never_started"}

	dc.StopAndRemoveContainer(t)
}

func newContainerWithBogusID(t *testing.T) *DatabaseContainer {
	t.Helper()
	client, err := docker.NewClientFromEnv()
	if err != nil {
		t.Skipf("no docker environment available: %v", err)
	}
	return &DatabaseContainer{
		Name:        "sc_test_no_such_container",
		client:      client,
		containerID: "sc_test_no_such_container",
	}
}

// errorRecorder implements [assert.TestingT] by counting reported failures.
type errorRecorder struct {
	mu       sync.Mutex
	failures int
}

func (r *errorRecorder) Errorf(string, ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures++
}

func (r *errorRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failures
}
