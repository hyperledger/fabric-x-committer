/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package snapshothasher

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/service/vc"
)

func TestSnapshotHashDeterministic(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	ctx, cancel := createContext(t)
	defer cancel()

	// Seed three namespaces with several keys each, plus committed tx statuses, so
	// the digest covers multiple ns_<id> tables AND tx_status.
	env.dbEnv.SeedState(t, seededState([]string{"1", "2", "3"}))

	ref := &committerpb.TxRef{BlockNum: 700100, TxNum: 0, TxId: "snap-hash-1"}
	h1 := env.createAndHashClone(ctx, t, ref)
	require.NotEmpty(t, h1)

	// Re-hashing the same immutable clone yields the identical digest.
	h2, err := env.hasher.hashSnapshotDatabase(ctx, vc.SnapshotDatabaseName(ref))
	require.NoError(t, err)
	require.Equal(t, h1, h2)

	// DIFFERENT state -> DIFFERENT hash: commit an extra row, then a fresh clone
	// must hash differently.
	env.dbEnv.SeedState(t, vc.StateFixture{
		Rows: map[string][]vc.KeyValue{"1": {{Key: []byte("key1.99"), Value: []byte("value1.99")}}},
	})

	ref2 := &committerpb.TxRef{BlockNum: 700110, TxNum: 0, TxId: "snap-hash-2"}
	require.NotEqual(t, h1, env.createAndHashClone(ctx, t, ref2))
}

// TestSnapshotHashExcludesUnregisteredNamespaces proves that rows in the
// `_snapshot` and `_checkpoint` system namespaces are EXCLUDED from the digest:
// those tables are not registered in ns__meta, so listHashedTables never hashes
// them. Excluding them is what keeps the digest stable while the hash job itself
// writes progress into `_snapshot`.
func TestSnapshotHashExcludesUnregisteredNamespaces(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	ctx, cancel := createContext(t)
	defer cancel()

	env.dbEnv.SeedState(t, seededState([]string{"1"}))

	baselineRef := &committerpb.TxRef{BlockNum: 710000, TxNum: 0, TxId: "snap-excl-base"}
	baselineHash := env.createAndHashClone(ctx, t, baselineRef)
	require.NotEmpty(t, baselineHash)

	// Write rows ONLY into the excluded system namespaces. Their tables exist, but
	// they are never registered in ns__meta, so a fresh clone's digest must be
	// unchanged. No user-namespace rows are added here, keeping this property
	// independent of the different-state property above.
	env.dbEnv.InsertRowDirectly(t, committerpb.SnapshotNamespaceID,
		vc.KeyValue{Key: []byte("excl-snap-key"), Value: []byte("excl-snap-val")})
	env.dbEnv.InsertRowDirectly(t, committerpb.CheckpointNamespaceID,
		vc.KeyValue{Key: []byte("excl-ckpt-key"), Value: []byte("excl-ckpt-val")})

	newRef := &committerpb.TxRef{BlockNum: 710100, TxNum: 0, TxId: "snap-excl-new"}
	require.Equal(t, baselineHash, env.createAndHashClone(ctx, t, newRef))
}

// TestSnapshotHashWithSingleWorker pins the clone pool's sizing rule: the pool gets
// exactly max-workers-for-hash connections, with none reserved for anything else. With
// a single worker the pool has a single connection, so the job can only complete if
// listHashedTables released its connection before the first worker asked for one.
// Reserving a spare connection would hide that ordering; a pool sized below the worker
// count would deadlock here rather than merely serialize.
//
// The digest must also be identical to the parallel one, since worker count must not
// affect the result.
func TestSnapshotHashWithSingleWorker(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	ctx, cancel := createContext(t)
	defer cancel()

	env.dbEnv.SeedState(t, seededState([]string{"1", "2", "3"}))

	ref := &committerpb.TxRef{BlockNum: 720000, TxNum: 0, TxId: "snap-hash-single-worker"}
	parallelHash := env.createAndHashClone(ctx, t, ref)
	require.NotEmpty(t, parallelHash)

	// Bounded well below the suite's timeout: an undersized pool starves the only
	// worker instead of returning an error, so without its own deadline this test
	// would report the regression as a hung package rather than a failure.
	serialCtx, serialCancel := context.WithTimeout(ctx, 90*time.Second)
	defer serialCancel()

	env.config.ResourceLimits.MaxWorkersForHash = 1
	serialHash, err := newHasher(env.config).hashSnapshotDatabase(serialCtx, vc.SnapshotDatabaseName(ref))
	require.NoError(t, err)
	require.Equal(t, parallelHash, serialHash)
}

// createAndHashClone creates the clone for ref (registering its cleanup drop) and
// returns its content hash.
func (env *testEnv) createAndHashClone(ctx context.Context, t *testing.T, ref *committerpb.TxRef) []byte {
	t.Helper()
	name := vc.SnapshotDatabaseName(ref)
	//nolint:contextcheck // the cleanup drop must run after the test context ends.
	env.dbEnv.CreateSnapshotClone(t, name)
	hash, err := env.hasher.hashSnapshotDatabase(ctx, name)
	require.NoError(t, err)
	return hash
}

// seededState returns committed state across the given namespaces, plus committed
// tx statuses, so a hashed clone has content in both ns_<id> tables and tx_status.
func seededState(nsIDs []string) vc.StateFixture {
	f := vc.StateFixture{NamespaceIDs: nsIDs, Rows: map[string][]vc.KeyValue{}}
	for _, ns := range nsIDs {
		for k := 1; k <= 5; k++ {
			f.Rows[ns] = append(f.Rows[ns], vc.KeyValue{
				Key:   fmt.Appendf(nil, "key%s.%d", ns, k),
				Value: fmt.Appendf(nil, "value%s.%d", ns, k),
			})
		}
	}
	for i := 1; i <= 3; i++ {
		f.TxStatuses = append(f.TxStatuses, &committerpb.TxRef{
			BlockNum: 700000, TxNum: uint32(i), TxId: fmt.Sprintf("snap-hash-seed-tx-%d", i),
		})
	}
	return f
}
