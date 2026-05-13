/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package vc

import (
	"fmt"
	"testing"

	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/stretchr/testify/require"
)

// sliceIterator is a test helper that implements StateIterator over in-memory slices.
type sliceIterator struct {
	keys     [][]byte
	values   [][]byte
	versions []uint64
	pos      int
	err      error
}

func newSliceIterator(keys, values [][]byte, versions []uint64) *sliceIterator {
	return &sliceIterator{keys: keys, values: values, versions: versions, pos: -1}
}

func (s *sliceIterator) Next() bool {
	if s.err != nil {
		return false
	}
	s.pos++
	return s.pos < len(s.keys)
}

func (s *sliceIterator) Key() []byte     { return s.keys[s.pos] }
func (s *sliceIterator) Value() []byte   { return s.values[s.pos] }
func (s *sliceIterator) Version() uint64 { return s.versions[s.pos] }
func (s *sliceIterator) Err() error      { return s.err }

// emptyIterator returns no records.
type emptyIterator struct{}

func (*emptyIterator) Next() bool      { return false }
func (*emptyIterator) Key() []byte     { return nil }
func (*emptyIterator) Value() []byte   { return nil }
func (*emptyIterator) Version() uint64 { return 0 }
func (*emptyIterator) Err() error      { return nil }

func newImporterWithSchema(t *testing.T) (*StateImporter, *DatabaseTestEnv) {
	t.Helper()
	env := NewDatabaseTestEnv(t)
	ctx, _ := createContext(t)
	importer := NewStateImporter(env.DB)
	require.NoError(t, importer.InitSchema(ctx))
	return importer, env
}

func TestStateImporter_InitSchema(t *testing.T) {
	t.Parallel()
	env := NewDatabaseTestEnv(t)
	ctx, _ := createContext(t)
	importer := NewStateImporter(env.DB)

	require.NoError(t, importer.InitSchema(ctx))

	// Verify system tables exist.
	tables := readTables(t, env.DB.pool)
	require.Contains(t, tables, "metadata")
	require.Contains(t, tables, "tx_status")
	require.Contains(t, tables, TableName(committerpb.MetaNamespaceID))
	require.Contains(t, tables, TableName(committerpb.ConfigNamespaceID))
}

func TestStateImporter_CreateNamespace(t *testing.T) {
	t.Parallel()
	importer, env := newImporterWithSchema(t)
	ctx, _ := createContext(t)

	require.NoError(t, importer.CreateNamespace(ctx, "myns"))

	tables := readTables(t, env.DB.pool)
	require.Contains(t, tables, TableName("myns"))

	methods := readMethods(t, env.DB.pool)
	require.Contains(t, methods, "insert_ns_myns")
	require.Contains(t, methods, "update_ns_myns")
	require.Contains(t, methods, "validate_reads_ns_myns")
}

func TestStateImporter_ImportNamespaceState(t *testing.T) {
	t.Parallel()
	importer, env := newImporterWithSchema(t)
	ctx, _ := createContext(t)

	require.NoError(t, importer.CreateNamespace(ctx, "importns"))

	keys := [][]byte{[]byte("k1"), []byte("k2"), []byte("k3")}
	values := [][]byte{[]byte("v1"), []byte("v2"), []byte("v3")}
	versions := []uint64{5, 10, 15}

	iter := newSliceIterator(keys, values, versions)
	count, err := importer.ImportNamespaceState(ctx, "importns", iter)
	require.NoError(t, err)
	require.Equal(t, int64(3), count)

	// Verify the data was written correctly.
	rows := env.FetchKeys(t, "importns", keys)
	require.Len(t, rows, 3)
	require.Equal(t, []byte("v1"), rows["k1"].Value)
	require.Equal(t, uint64(5), rows["k1"].Version)
	require.Equal(t, []byte("v2"), rows["k2"].Value)
	require.Equal(t, uint64(10), rows["k2"].Version)
	require.Equal(t, []byte("v3"), rows["k3"].Value)
	require.Equal(t, uint64(15), rows["k3"].Version)
}

func TestStateImporter_ImportNamespaceState_Empty(t *testing.T) {
	t.Parallel()
	importer, _ := newImporterWithSchema(t)
	ctx, _ := createContext(t)

	require.NoError(t, importer.CreateNamespace(ctx, "emptyns"))

	count, err := importer.ImportNamespaceState(ctx, "emptyns", &emptyIterator{})
	require.NoError(t, err)
	require.Equal(t, int64(0), count)
}

func TestStateImporter_ImportNamespaceState_LargeBatch(t *testing.T) {
	t.Parallel()
	importer, _ := newImporterWithSchema(t)
	ctx, _ := createContext(t)

	require.NoError(t, importer.CreateNamespace(ctx, "largens"))

	const numKeys = 10_000
	keys := make([][]byte, numKeys)
	values := make([][]byte, numKeys)
	versions := make([]uint64, numKeys)
	for i := range numKeys {
		keys[i] = []byte(fmt.Sprintf("key-%06d", i))
		values[i] = []byte(fmt.Sprintf("val-%06d", i))
		versions[i] = uint64(i)
	}

	iter := newSliceIterator(keys, values, versions)
	count, err := importer.ImportNamespaceState(ctx, "largens", iter)
	require.NoError(t, err)
	require.Equal(t, int64(numKeys), count)

	// Spot-check a few rows.
	require.NoError(t, importer.VerifyNamespaceRowCount(ctx, "largens", numKeys))
}

func TestStateImporter_ImportNamespaceState_NonexistentTable(t *testing.T) {
	t.Parallel()
	importer, _ := newImporterWithSchema(t)
	ctx, _ := createContext(t)

	// Table was never created — import should fail.
	iter := newSliceIterator([][]byte{[]byte("k")}, [][]byte{[]byte("v")}, []uint64{0})
	_, err := importer.ImportNamespaceState(ctx, "no_such_ns", iter)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no_such_ns")
}

func TestStateImporter_ImportPolicy(t *testing.T) {
	t.Parallel()
	importer, env := newImporterWithSchema(t)
	ctx, _ := createContext(t)

	require.NoError(t, importer.CreateNamespace(ctx, "policyns"))
	policyBytes := []byte("serialized-policy-data")

	require.NoError(t, importer.ImportPolicy(ctx, "policyns", policyBytes))

	// Verify policy was written to __meta namespace.
	rows := env.FetchKeys(t, committerpb.MetaNamespaceID, [][]byte{[]byte("policyns")})
	require.Len(t, rows, 1)
	require.Equal(t, policyBytes, rows["policyns"].Value)
}

func TestStateImporter_ImportPolicy_Duplicate(t *testing.T) {
	t.Parallel()
	importer, _ := newImporterWithSchema(t)
	ctx, _ := createContext(t)

	require.NoError(t, importer.ImportPolicy(ctx, "dupns", []byte("policy1")))

	// Second import for the same namespace should fail.
	err := importer.ImportPolicy(ctx, "dupns", []byte("policy2"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "already exists")
}

func TestStateImporter_SetBlockHeight(t *testing.T) {
	t.Parallel()
	importer, env := newImporterWithSchema(t)
	ctx, _ := createContext(t)

	require.NoError(t, importer.SetBlockHeight(ctx, 42))

	// Verify via the existing database method.
	blkRef, err := env.DB.getNextBlockNumberToCommit(ctx)
	require.NoError(t, err)
	require.Equal(t, uint64(43), blkRef.Number) // next = last + 1
}

func TestStateImporter_SetBlockHeight_Zero(t *testing.T) {
	t.Parallel()
	importer, env := newImporterWithSchema(t)
	ctx, _ := createContext(t)

	require.NoError(t, importer.SetBlockHeight(ctx, 0))

	blkRef, err := env.DB.getNextBlockNumberToCommit(ctx)
	require.NoError(t, err)
	require.Equal(t, uint64(1), blkRef.Number)
}

func TestStateImporter_VerifyNamespaceRowCount_Match(t *testing.T) {
	t.Parallel()
	importer, _ := newImporterWithSchema(t)
	ctx, _ := createContext(t)

	require.NoError(t, importer.CreateNamespace(ctx, "verifyns"))

	iter := newSliceIterator(
		[][]byte{[]byte("a"), []byte("b")},
		[][]byte{[]byte("1"), []byte("2")},
		[]uint64{0, 0},
	)
	_, err := importer.ImportNamespaceState(ctx, "verifyns", iter)
	require.NoError(t, err)

	require.NoError(t, importer.VerifyNamespaceRowCount(ctx, "verifyns", 2))
}

func TestStateImporter_VerifyNamespaceRowCount_Mismatch(t *testing.T) {
	t.Parallel()
	importer, _ := newImporterWithSchema(t)
	ctx, _ := createContext(t)

	require.NoError(t, importer.CreateNamespace(ctx, "mismatchns"))

	iter := newSliceIterator(
		[][]byte{[]byte("a")},
		[][]byte{[]byte("1")},
		[]uint64{0},
	)
	_, err := importer.ImportNamespaceState(ctx, "mismatchns", iter)
	require.NoError(t, err)

	err = importer.VerifyNamespaceRowCount(ctx, "mismatchns", 5)
	require.Error(t, err)
	require.Contains(t, err.Error(), "row count mismatch")
	require.Contains(t, err.Error(), "expected 5")
	require.Contains(t, err.Error(), "got 1")
}

func TestStateImporter_VerifyImport(t *testing.T) {
	t.Parallel()
	importer, _ := newImporterWithSchema(t)
	ctx, _ := createContext(t)

	require.NoError(t, importer.CreateNamespace(ctx, "ns_a"))
	require.NoError(t, importer.CreateNamespace(ctx, "ns_b"))

	iterA := newSliceIterator(
		[][]byte{[]byte("k1"), []byte("k2")},
		[][]byte{[]byte("v1"), []byte("v2")},
		[]uint64{0, 1},
	)
	_, err := importer.ImportNamespaceState(ctx, "ns_a", iterA)
	require.NoError(t, err)

	iterB := newSliceIterator(
		[][]byte{[]byte("k3")},
		[][]byte{[]byte("v3")},
		[]uint64{0},
	)
	_, err = importer.ImportNamespaceState(ctx, "ns_b", iterB)
	require.NoError(t, err)

	require.NoError(t, importer.SetBlockHeight(ctx, 100))

	summary := &ImportSummary{
		NamespaceRowCounts: map[string]int64{
			"ns_a": 2,
			"ns_b": 1,
		},
		BlockNumber: 100,
		TotalKeys:   3,
	}

	require.NoError(t, importer.VerifyImport(ctx, summary))
}

func TestStateImporter_VerifyImport_BlockHeightMismatch(t *testing.T) {
	t.Parallel()
	importer, _ := newImporterWithSchema(t)
	ctx, _ := createContext(t)

	require.NoError(t, importer.SetBlockHeight(ctx, 50))

	summary := &ImportSummary{
		NamespaceRowCounts: map[string]int64{},
		BlockNumber:        99, // does not match
	}

	err := importer.VerifyImport(ctx, summary)
	require.Error(t, err)
	require.Contains(t, err.Error(), "block height mismatch")
}

func TestStateImporter_EndToEnd_MultiNamespace(t *testing.T) {
	t.Parallel()
	importer, env := newImporterWithSchema(t)
	ctx, _ := createContext(t)

	// Simulate importing a Fabric snapshot with 3 namespaces.
	namespaces := []string{"chaincode1", "chaincode2", "lifecycle"}
	for _, ns := range namespaces {
		require.NoError(t, importer.CreateNamespace(ctx, ns))
		require.NoError(t, importer.ImportPolicy(ctx, ns, []byte("policy-for-"+ns)))
	}

	// Import state for each namespace.
	summary := &ImportSummary{
		NamespaceRowCounts: make(map[string]int64),
	}

	for i, ns := range namespaces {
		numKeys := (i + 1) * 100
		keys := make([][]byte, numKeys)
		values := make([][]byte, numKeys)
		versions := make([]uint64, numKeys)
		for j := range numKeys {
			keys[j] = []byte(fmt.Sprintf("%s-key-%04d", ns, j))
			values[j] = []byte(fmt.Sprintf("%s-val-%04d", ns, j))
			versions[j] = uint64(j)
		}

		count, err := importer.ImportNamespaceState(ctx, ns, newSliceIterator(keys, values, versions))
		require.NoError(t, err)
		require.Equal(t, int64(numKeys), count)
		summary.NamespaceRowCounts[ns] = int64(numKeys)
		summary.TotalKeys += int64(numKeys)
	}

	// Set block height.
	summary.BlockNumber = 500
	require.NoError(t, importer.SetBlockHeight(ctx, summary.BlockNumber))

	// Full verification.
	require.NoError(t, importer.VerifyImport(ctx, summary))

	// Verify policies are readable by the existing readNamespacePolicies method.
	policies, err := env.DB.readNamespacePolicies(ctx)
	require.NoError(t, err)
	require.Len(t, policies.Policies, len(namespaces))

	// Verify data survives and is usable by the normal pipeline — spot-check reads.
	for _, ns := range namespaces {
		rows := env.FetchKeys(t, ns, [][]byte{[]byte(ns + "-key-0000")})
		require.Len(t, rows, 1)
		require.Equal(t, []byte(ns+"-val-0000"), rows[ns+"-key-0000"].Value)
	}
}
