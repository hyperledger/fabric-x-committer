/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package snapshothasher

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/service/vc"
	"github.com/hyperledger/fabric-x-committer/utils/snapshotstate"
	"github.com/hyperledger/fabric-x-committer/utils/statedb"
	"github.com/hyperledger/fabric-x-committer/utils/testdb"
)

func TestMain(m *testing.M) {
	testdb.RunTestMain(m)
}

// testPollInterval keeps a scheduler tick observable inside a test instead of
// after a production-length wait.
const testPollInterval = 2 * time.Second

// testEnv wires a scheduler and a hasher onto the same database a
// validator-committer commits into, which is the whole deployment this service
// assumes: the VC makes records durable, and this process discovers them.
type testEnv struct {
	dbEnv     *vc.DatabaseTestEnv
	config    *Config
	metrics   *perfMetrics
	hasher    *hasher
	state     *snapshotstate.StateManager
	scheduler *scheduler
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dbEnv := vc.NewDatabaseTestEnv(t)
	// YugabyteDB's clone prerequisite, made explicit: cloning requires a snapshot
	// schedule on the source keyspace.
	testdb.EnsureSnapshotSchedule(t, dbEnv.DBConf.Database)

	config := &Config{
		Database:     dbEnv.DBConf,
		PollInterval: testPollInterval,
		ResourceLimits: &ResourceLimitsConfig{
			MaxWorkersForHash: 4,
			HashBatchSize:     1000,
		},
	}

	pool, err := statedb.NewPool(t.Context(), config.Database)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	state := snapshotstate.NewStateManager(pool, config.Database.Retry)
	hasher := newHasher(config)
	metrics := newSnapshotHasherMetrics()
	return &testEnv{
		dbEnv:   dbEnv,
		config:  config,
		metrics: metrics,
		hasher:  hasher,
		state:   state,
		scheduler: newScheduler(&schedulerConfig{
			state:        state,
			hasher:       hasher,
			metrics:      metrics,
			pollInterval: config.PollInterval,
		}),
	}
}

func createContext(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	t.Cleanup(cancel)
	return ctx, cancel
}
