/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package vc

import (
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
)

func TestBulkImportSingleNamespace(t *testing.T) {
	t.Parallel()
	env := newDatabaseTestEnvWithTablesSetup(t)

	data := []*BulkImportData{
		{
			NsID:     "mycc",
			Keys:     [][]byte{[]byte("key1"), []byte("key2"), []byte("key3")},
			Values:   [][]byte{[]byte("val1"), []byte("val2"), []byte("val3")},
			Versions: []uint64{5, 10, 15},
		},
	}

	result, err := env.DB.bulkImport(t.Context(), data)
	require.NoError(t, err)
	require.Equal(t, uint64(3), result.NamespaceCounts["mycc"])

	// Verify data is readable.
	rows := env.FetchKeys(t, "mycc", [][]byte{[]byte("key1"), []byte("key2"), []byte("key3")})
	require.Len(t, rows, 3)
	assert.Equal(t, []byte("val1"), rows["key1"].Value)
	assert.Equal(t, uint64(5), rows["key1"].Version)
	assert.Equal(t, []byte("val2"), rows["key2"].Value)
	assert.Equal(t, uint64(10), rows["key2"].Version)
	assert.Equal(t, []byte("val3"), rows["key3"].Value)
	assert.Equal(t, uint64(15), rows["key3"].Version)
}

func TestBulkImportMultipleNamespaces(t *testing.T) {
	t.Parallel()
	env := newDatabaseTestEnvWithTablesSetup(t)

	data := []*BulkImportData{
		{
			NsID:     "ns_alpha",
			Keys:     [][]byte{[]byte("a1"), []byte("a2")},
			Values:   [][]byte{[]byte("va1"), []byte("va2")},
			Versions: []uint64{1, 2},
		},
		{
			NsID:     "ns_beta",
			Keys:     [][]byte{[]byte("b1"), []byte("b2"), []byte("b3")},
			Values:   [][]byte{[]byte("vb1"), []byte("vb2"), []byte("vb3")},
			Versions: []uint64{3, 4, 5},
		},
		{
			NsID:     "ns_gamma",
			Keys:     [][]byte{[]byte("g1")},
			Values:   [][]byte{[]byte("vg1")},
			Versions: []uint64{0},
		},
	}

	result, err := env.DB.bulkImport(t.Context(), data)
	require.NoError(t, err)
	require.Len(t, result.NamespaceCounts, 3)
	assert.Equal(t, uint64(2), result.NamespaceCounts["ns_alpha"])
	assert.Equal(t, uint64(3), result.NamespaceCounts["ns_beta"])
	assert.Equal(t, uint64(1), result.NamespaceCounts["ns_gamma"])

	// Verify each namespace.
	env.tableExists(t, "ns_alpha")
	env.tableExists(t, "ns_beta")
	env.tableExists(t, "ns_gamma")

	rows := env.FetchKeys(t, "ns_beta", [][]byte{[]byte("b1"), []byte("b2"), []byte("b3")})
	require.Len(t, rows, 3)
	assert.Equal(t, []byte("vb2"), rows["b2"].Value)
	assert.Equal(t, uint64(4), rows["b2"].Version)
}

func TestBulkImportMultipleChunksForSameNamespace(t *testing.T) {
	t.Parallel()
	env := newDatabaseTestEnvWithTablesSetup(t)

	// Simulate chunked streaming: two BulkImportData entries for the same namespace.
	data := []*BulkImportData{
		{
			NsID:     "chunked",
			Keys:     [][]byte{[]byte("c1"), []byte("c2")},
			Values:   [][]byte{[]byte("vc1"), []byte("vc2")},
			Versions: []uint64{0, 1},
		},
		{
			NsID:     "chunked",
			Keys:     [][]byte{[]byte("c3"), []byte("c4")},
			Values:   [][]byte{[]byte("vc3"), []byte("vc4")},
			Versions: []uint64{2, 3},
		},
	}

	result, err := env.DB.bulkImport(t.Context(), data)
	require.NoError(t, err)
	assert.Equal(t, uint64(4), result.NamespaceCounts["chunked"])

	rows := env.FetchKeys(t, "chunked",
		[][]byte{[]byte("c1"), []byte("c2"), []byte("c3"), []byte("c4")})
	require.Len(t, rows, 4)
	assert.Equal(t, uint64(3), rows["c4"].Version)
}

func TestBulkImportCreatesTablesAndFunctions(t *testing.T) {
	t.Parallel()
	env := newDatabaseTestEnvWithTablesSetup(t)

	data := []*BulkImportData{
		{
			NsID:     "newns",
			Keys:     [][]byte{[]byte("k1")},
			Values:   [][]byte{[]byte("v1")},
			Versions: []uint64{0},
		},
	}

	_, err := env.DB.bulkImport(t.Context(), data)
	require.NoError(t, err)

	// Verify the table exists.
	env.tableExists(t, "newns")

	// Verify the SQL functions were created by using them.
	// validate_reads should find no conflicts for the imported data.
	v := applicationpb.NewVersion
	conflicts, err := env.DB.validateNamespaceReads(t.Context(), "newns", &reads{
		keys:     [][]byte{[]byte("k1")},
		versions: []*uint64{v(0)},
	})
	require.NoError(t, err)
	assert.Empty(t, conflicts.keys)
}

func TestBulkImportEmptyData(t *testing.T) {
	t.Parallel()
	env := newDatabaseTestEnvWithTablesSetup(t)

	result, err := env.DB.bulkImport(t.Context(), nil)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Empty(t, result.NamespaceCounts)

	result, err = env.DB.bulkImport(t.Context(), []*BulkImportData{})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Empty(t, result.NamespaceCounts)
}

func TestBulkImportEmptyNamespaceData(t *testing.T) {
	t.Parallel()
	env := newDatabaseTestEnvWithTablesSetup(t)

	data := []*BulkImportData{
		{
			NsID:     "emptyns",
			Keys:     [][]byte{},
			Values:   [][]byte{},
			Versions: []uint64{},
		},
	}

	result, err := env.DB.bulkImport(t.Context(), data)
	require.NoError(t, err)
	assert.Equal(t, uint64(0), result.NamespaceCounts["emptyns"])
}

func TestBulkImportRejectsNonEmptyDatabase(t *testing.T) {
	t.Parallel()
	env := newDatabaseTestEnvWithTablesSetup(t)

	// Mark the database as having committed blocks.
	blockRef := &servicepb.BlockRef{Number: 10}
	require.NoError(t, env.DB.setLastCommittedBlockNumber(t.Context(), blockRef))

	data := []*BulkImportData{
		{
			NsID:     "mycc",
			Keys:     [][]byte{[]byte("k1")},
			Values:   [][]byte{[]byte("v1")},
			Versions: []uint64{0},
		},
	}

	_, err := env.DB.bulkImport(t.Context(), data)
	require.ErrorIs(t, err, ErrDatabaseNotEmpty)
}

func TestBulkImportRejectsMismatchedSliceLengths(t *testing.T) {
	t.Parallel()
	env := newDatabaseTestEnvWithTablesSetup(t)

	data := []*BulkImportData{
		{
			NsID:     "mycc",
			Keys:     [][]byte{[]byte("k1"), []byte("k2")},
			Values:   [][]byte{[]byte("v1")}, // one short
			Versions: []uint64{0, 1},
		},
	}

	_, err := env.DB.bulkImport(t.Context(), data)
	require.Error(t, err)
	require.Contains(t, err.Error(), "mismatched slice lengths")
}

func TestBulkImportRejectsEmptyNamespaceID(t *testing.T) {
	t.Parallel()
	env := newDatabaseTestEnvWithTablesSetup(t)

	data := []*BulkImportData{
		{
			NsID:     "",
			Keys:     [][]byte{[]byte("k1")},
			Values:   [][]byte{[]byte("v1")},
			Versions: []uint64{0},
		},
	}

	_, err := env.DB.bulkImport(t.Context(), data)
	require.Error(t, err)
	require.Contains(t, err.Error(), "empty namespace ID")
}

func TestBulkImportAtomicityOnDuplicateKeys(t *testing.T) {
	t.Parallel()
	env := newDatabaseTestEnvWithTablesSetup(t)

	// The second entry for the same namespace has a duplicate key which will
	// cause COPY to fail with a unique constraint violation, rolling back both.
	data := []*BulkImportData{
		{
			NsID:     "atomicns",
			Keys:     [][]byte{[]byte("k1"), []byte("k2")},
			Values:   [][]byte{[]byte("v1"), []byte("v2")},
			Versions: []uint64{0, 0},
		},
		{
			NsID:     "atomicns",
			Keys:     [][]byte{[]byte("k1")}, // duplicate key
			Values:   [][]byte{[]byte("v1dup")},
			Versions: []uint64{1},
		},
	}

	_, err := env.DB.bulkImport(t.Context(), data)
	require.Error(t, err)

	// Verify the table was not created (transaction rolled back).
	var count int
	row := env.DB.pool.QueryRow(t.Context(),
		"SELECT count(*) FROM information_schema.tables WHERE table_name = $1", TableName("atomicns"))
	require.NoError(t, row.Scan(&count))
	assert.Equal(t, 0, count)
}

func TestBulkImportCompatibleWithNormalPipeline(t *testing.T) {
	t.Parallel()
	env := newDatabaseTestEnvWithTablesSetup(t)

	// Step 1: Bulk import initial state.
	importData := []*BulkImportData{
		{
			NsID:     ns1,
			Keys:     [][]byte{[]byte("key1"), []byte("key2")},
			Values:   [][]byte{[]byte("val1"), []byte("val2")},
			Versions: []uint64{0, 0},
		},
	}

	result, err := env.DB.bulkImport(t.Context(), importData)
	require.NoError(t, err)
	require.Equal(t, uint64(2), result.NamespaceCounts[ns1])

	// Step 2: Validate that reads work against the bulk-imported state.
	// Reading key1 at version 0 should find no conflicts.
	v := applicationpb.NewVersion
	conflicts, err := env.DB.validateNamespaceReads(t.Context(), ns1, &reads{
		keys:     [][]byte{[]byte("key1"), []byte("key2")},
		versions: []*uint64{v(0), v(0)},
	})
	require.NoError(t, err)
	require.Empty(t, conflicts.keys, "expected no read conflicts for bulk-imported data")

	// Reading key1 at wrong version should find a conflict.
	conflicts, err = env.DB.validateNamespaceReads(t.Context(), ns1, &reads{
		keys:     [][]byte{[]byte("key1")},
		versions: []*uint64{v(99)},
	})
	require.NoError(t, err)
	require.Len(t, conflicts.keys, 1, "expected a conflict for wrong version")

	// Step 3: Commit an update via the normal pipeline path.
	// This uses the same code path as the transactionCommitter.
	txID := TxID("tx-after-import")
	height := servicepb.NewHeight(1, 0)

	states := &statesToBeCommitted{
		updateWrites: namespaceToWrites{
			ns1: {
				keys:     [][]byte{[]byte("key1")},
				values:   [][]byte{[]byte("val1-updated")},
				versions: []uint64{1},
			},
		},
		newWrites: namespaceToWrites{},
		batchStatus: &committerpb.TxStatusBatch{
			Status: []*committerpb.TxStatus{
				height.WithStatus(string(txID), committerpb.Status_COMMITTED),
			},
		},
		txIDToHeight: transactionIDToHeight{txID: height},
	}

	require.NoError(t, env.DB.retry.Execute(t.Context(), func() error {
		c, d, e := env.DB.commit(t.Context(), states)
		require.Empty(t, c)
		require.Empty(t, d)
		return e
	}))

	// Verify the update was applied.
	rows := env.FetchKeys(t, ns1, [][]byte{[]byte("key1")})
	require.Len(t, rows, 1)
	assert.Equal(t, []byte("val1-updated"), rows["key1"].Value)
	assert.Equal(t, uint64(1), rows["key1"].Version)
}

func TestBulkImportLargeDataset(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping large dataset test in short mode")
	}
	t.Parallel()
	env := newDatabaseTestEnvWithTablesSetup(t)

	const numKeys = 50_000
	keys := make([][]byte, numKeys)
	values := make([][]byte, numKeys)
	versions := make([]uint64, numKeys)
	for i := range numKeys {
		keys[i] = binary.BigEndian.AppendUint64([]byte("key-"), uint64(i))
		values[i] = fmt.Appendf(nil, "value-%d", i)
		versions[i] = uint64(i % 1000)
	}

	data := []*BulkImportData{
		{NsID: "largens", Keys: keys, Values: values, Versions: versions},
	}

	result, err := env.DB.bulkImport(t.Context(), data)
	require.NoError(t, err)
	assert.Equal(t, uint64(numKeys), result.NamespaceCounts["largens"])

	// Spot-check a few keys.
	sampleKeys := [][]byte{keys[0], keys[numKeys/2], keys[numKeys-1]}
	rows := env.FetchKeys(t, "largens", sampleKeys)
	require.Len(t, rows, 3)
}

func TestBulkImportIntoSystemNamespace(t *testing.T) {
	t.Parallel()
	env := newDatabaseTestEnvWithTablesSetup(t)

	// The system namespace tables (_meta, _config) already exist from setupSystemTablesAndNamespaces.
	// Bulk import should be able to insert into them using CREATE TABLE IF NOT EXISTS.
	data := []*BulkImportData{
		{
			NsID:     committerpb.MetaNamespaceID,
			Keys:     [][]byte{[]byte("chaincode1"), []byte("chaincode2")},
			Values:   [][]byte{[]byte("policy1"), []byte("policy2")},
			Versions: []uint64{0, 0},
		},
	}

	result, err := env.DB.bulkImport(t.Context(), data)
	require.NoError(t, err)
	assert.Equal(t, uint64(2), result.NamespaceCounts[committerpb.MetaNamespaceID])

	rows := env.FetchKeys(t, committerpb.MetaNamespaceID,
		[][]byte{[]byte("chaincode1"), []byte("chaincode2")})
	require.Len(t, rows, 2)
	assert.Equal(t, []byte("policy1"), rows["chaincode1"].Value)
}
