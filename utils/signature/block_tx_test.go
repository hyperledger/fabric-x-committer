/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package signature_test

import (
	"encoding/asn1"
	"math/rand"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
	"github.com/hyperledger/fabric-x-committer/api/types"
	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/utils/signature"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

func BenchmarkDigest(b *testing.B) {
	g := workload.StartGenerator(b, workload.DefaultProfile(8))
	txs := g.NextN(b.Context(), b.N)

	var resBench []byte
	var errBench error

	b.ResetTimer()
	for _, tx := range txs {
		resBench, errBench = signature.DigestTxNamespace(tx, 0)
	}
	b.StopTimer()
	require.NoError(b, errBench)
	require.NotNil(b, resBench)
}

func TestAsnMarshal(t *testing.T) {
	t.Parallel()

	txs := []*protoblocktx.Tx{txTestCases}
	// We test against the generated load to enforce a coupling between different parts of the system.
	g := workload.StartGenerator(t, workload.DefaultProfile(8))
	txs = append(txs, g.NextN(t.Context(), 1024)...)

	for _, tx := range txs {
		tx := tx
		// We don't serialize the signature.
		tx.Signatures = nil
		t.Run(tx.Id, func(t *testing.T) {
			t.Parallel()
			for i, ns := range tx.Namespaces {
				nsIdx := i
				t.Run(ns.NsId, func(t *testing.T) {
					t.Parallel()
					requireASN1Marshal(t, tx, nsIdx)
				})
			}

			// Test that we can reconstruct the TX from the marshalled version.
			// This ensures we didn't lose any data in the translation.
			t.Run("verify reconstruction of the TX", func(t *testing.T) {
				t.Parallel()
				translatedNamespace := make([][]byte, len(tx.Namespaces))
				for i := range tx.Namespaces {
					var err error
					translatedNamespace[i], err = signature.ASN1MarshalTxNamespace(tx, i)
					require.NoError(t, err)
				}
				actualTx := reconstructTX(t, translatedNamespace)
				test.RequireProtoEqual(t, tx, actualTx)
			})
		})
	}
}

func FuzzASN1MarshalTxNamespace(f *testing.F) {
	i := uint32(1024)
	for _, ns := range txTestCases.Namespaces {
		//nolint:gosec // false positive; safe integer conversion.
		for _, r := range ns.ReadsOnly {
			f.Add(
				txTestCases.Id, ns.NsId, ns.NsVersion, signature.ProtoToAsnVersion(r.Version),
				uint32(len(ns.ReadsOnly)), uint32(len(ns.ReadWrites)), uint32(len(ns.BlindWrites)),
				i, int64(1234+i),
			)
			i++
		}
		//nolint:gosec // false positive; safe integer conversion.
		for _, r := range ns.ReadWrites {
			f.Add(
				txTestCases.Id, ns.NsId, ns.NsVersion, signature.ProtoToAsnVersion(r.Version),
				uint32(len(ns.ReadsOnly)), uint32(len(ns.ReadWrites)), uint32(len(ns.BlindWrites)),
				i, int64(1234+i),
			)
			i++
		}
	}
	f.Fuzz(func(
		t *testing.T,
		id, nsID string, nsVersion uint64, readVersion int64,
		rCount, rwCount, wCount uint32,
		maxSize uint32, seed int64,
	) {
		tx := generateTX(t, id, nsID, nsVersion, readVersion, rCount, rwCount, wCount, maxSize, seed)
		derBytes := requireASN1Marshal(t, tx, 0)
		actualTx := reconstructTX(t, [][]byte{derBytes})
		test.RequireProtoEqual(t, tx, actualTx)
	})
}

func requireASN1Marshal(t *testing.T, tx *protoblocktx.Tx, nsIdx int) []byte {
	t.Helper()
	translated, err := signature.TranslateTx(tx, nsIdx)
	require.NoError(t, err)

	// The marshalled object cannot distinguish between empty and nil slice.
	// So, we convert all nils to empty slices to allow comparison with the unmarshalled object.
	for i := range translated.ReadWrites {
		rw := &translated.ReadWrites[i]
		if rw.Value == nil {
			rw.Value = make([]byte, 0)
		}
	}
	for i := range translated.BlindWrites {
		w := &translated.BlindWrites[i]
		if w.Value == nil {
			w.Value = make([]byte, 0)
		}
	}

	derBytes, err := signature.ASN1MarshalTxNamespace(tx, nsIdx)
	require.NoError(t, err)

	actual := &signature.TxWithNamespace{}
	_, err = asn1.Unmarshal(derBytes, actual)
	require.NoError(t, err)
	require.Equal(t, translated, actual)
	return derBytes
}

// reconstructTX unmarshal the given namespaces and reconstruct a TX.
// Any change to [*protoblocktx.Tx] requires a change to this method.
func reconstructTX(t *testing.T, namespaces [][]byte) *protoblocktx.Tx {
	t.Helper()
	tx := &protoblocktx.Tx{}

	for i, nsDer := range namespaces {
		translated := &signature.TxWithNamespace{}
		_, err := asn1.Unmarshal(nsDer, translated)
		require.NoError(t, err)

		if i == 0 {
			tx.Id = translated.TxID
		} else {
			require.Equal(t, tx.Id, translated.TxID)
		}

		nsVer := signature.AsnToProtoVersion(translated.NamespaceVersion)
		require.NotNil(t, nsVer)
		ns := &protoblocktx.TxNamespace{
			NsId:        translated.NamespaceID,
			NsVersion:   *nsVer,
			ReadsOnly:   make([]*protoblocktx.Read, len(translated.ReadsOnly)),
			ReadWrites:  make([]*protoblocktx.ReadWrite, len(translated.ReadWrites)),
			BlindWrites: make([]*protoblocktx.Write, len(translated.BlindWrites)),
		}
		tx.Namespaces = append(tx.Namespaces, ns)

		for j, read := range translated.ReadsOnly {
			ns.ReadsOnly[j] = &protoblocktx.Read{
				Key:     read.Key,
				Version: signature.AsnToProtoVersion(read.Version),
			}
		}
		for j, rw := range translated.ReadWrites {
			ns.ReadWrites[j] = &protoblocktx.ReadWrite{
				Key:     rw.Key,
				Value:   rw.Value,
				Version: signature.AsnToProtoVersion(rw.Version),
			}
		}
		for j, w := range translated.BlindWrites {
			ns.BlindWrites[j] = &protoblocktx.Write{
				Key:   w.Key,
				Value: w.Value,
			}
		}
	}

	return tx
}

// generateTX creates a TX with a single namespace given the input parameters.
func generateTX( //nolint:revive // required parameters.
	t *testing.T,
	id, nsID string, nsVersion uint64, readVersion int64,
	rCount, rwCount, wCount uint32,
	maxSize uint32, seed int64,
) *protoblocktx.Tx {
	t.Helper()
	if !utf8.ValidString(id) || !utf8.ValidString(nsID) {
		t.Skip("invalid UTF8")
	}
	tx := &protoblocktx.Tx{
		Id: id,
		Namespaces: []*protoblocktx.TxNamespace{
			{
				NsId:        nsID,
				NsVersion:   nsVersion,
				ReadsOnly:   make([]*protoblocktx.Read, rCount),
				ReadWrites:  make([]*protoblocktx.ReadWrite, rwCount),
				BlindWrites: make([]*protoblocktx.Write, wCount),
			},
		},
	}
	rnd := rand.New(rand.NewSource(seed))
	for i := range tx.Namespaces[0].ReadsOnly {
		read := &protoblocktx.Read{
			Key:     make([]byte, rnd.Intn(int(maxSize))),
			Version: signature.AsnToProtoVersion(readVersion),
		}
		_, err := rnd.Read(read.Key)
		require.NoError(t, err)
		tx.Namespaces[0].ReadsOnly[i] = read
	}
	for i := range tx.Namespaces[0].ReadWrites {
		readWrite := &protoblocktx.ReadWrite{
			Key:     make([]byte, rnd.Intn(int(maxSize))),
			Value:   make([]byte, rnd.Intn(int(maxSize))),
			Version: signature.AsnToProtoVersion(readVersion),
		}
		_, err := rnd.Read(readWrite.Key)
		require.NoError(t, err)
		_, err = rnd.Read(readWrite.Value)
		require.NoError(t, err)
		tx.Namespaces[0].ReadWrites[i] = readWrite
	}
	for i := range tx.Namespaces[0].BlindWrites {
		write := &protoblocktx.Write{
			Key:   make([]byte, rnd.Intn(int(maxSize))),
			Value: make([]byte, rnd.Intn(int(maxSize))),
		}
		_, err := rnd.Read(write.Key)
		require.NoError(t, err)
		_, err = rnd.Read(write.Value)
		require.NoError(t, err)
		tx.Namespaces[0].BlindWrites[i] = write
	}
	return tx
}

var txTestCases = &protoblocktx.Tx{
	Id: "tx-1",
	Namespaces: []*protoblocktx.TxNamespace{
		{
			NsId:      "empty",
			NsVersion: 1,
		},
		{
			NsId:      "only reads",
			NsVersion: 2,
			ReadsOnly: []*protoblocktx.Read{
				{
					Key:     []byte{1},
					Version: types.Version(2),
				},
				{
					Key:     []byte{3, 4, 5},
					Version: types.Version(0),
				},
			},
		},
		{
			NsId:      "only reads with nil version",
			NsVersion: 2,
			ReadsOnly: []*protoblocktx.Read{
				{
					Key:     []byte{1},
					Version: types.Version(2),
				},
				{
					Key:     []byte{3, 4, 5},
					Version: types.Version(0),
				},
				{
					Key:     []byte{7, 8, 9},
					Version: nil,
				},
			},
		},
		{
			NsId:      "only read-write",
			NsVersion: 3,
			ReadWrites: []*protoblocktx.ReadWrite{
				{
					Key:     []byte{1},
					Version: types.Version(2),
					Value:   []byte{3},
				},
				{
					Key:     []byte{5},
					Version: types.Version(0),
					Value:   []byte{6},
				},
			},
		},
		{
			NsId:      "only read-write with nil value or version",
			NsVersion: 3,
			ReadWrites: []*protoblocktx.ReadWrite{
				{
					Key:     []byte{1},
					Version: types.Version(2),
					Value:   []byte{3},
				},
				{
					Key:     []byte{7},
					Version: nil,
					Value:   []byte{8},
				},
				{
					Key:     []byte{9},
					Version: types.Version(3),
					Value:   nil,
				},
				{
					Key:     []byte{10},
					Version: nil,
					Value:   nil,
				},
			},
		},
		{
			NsId:      "only blind writes",
			NsVersion: 4,
			BlindWrites: []*protoblocktx.Write{
				{
					Key:   []byte{5},
					Value: []byte{6, 7},
				},
				{
					Key:   []byte{10},
					Value: make([]byte, 0),
				},
			},
		},
		{
			NsId:      "only blind writes with nil value",
			NsVersion: 4,
			BlindWrites: []*protoblocktx.Write{
				{
					Key:   []byte{5},
					Value: []byte{6, 7},
				},
				{
					Key:   []byte{10},
					Value: make([]byte, 0),
				},
				{
					Key:   []byte{11},
					Value: nil,
				},
			},
		},
		{
			NsId:      "all",
			NsVersion: 5,
			ReadsOnly: []*protoblocktx.Read{
				{
					Key:     []byte{6},
					Version: types.Version(7),
				},
				{
					Key:     []byte{9, 10, 11},
					Version: types.Version(12),
				},
			},
			ReadWrites: []*protoblocktx.ReadWrite{
				{
					Key:     []byte{100},
					Version: types.Version(1),
					Value:   []byte{2},
				},
				{
					Key:     []byte{5},
					Version: types.Version(10),
					Value:   []byte{13},
				},
			},
			BlindWrites: []*protoblocktx.Write{
				{
					Key:   []byte{1, 2, 3},
					Value: []byte{100, 101, 102},
				},
				{
					Key:   []byte{5},
					Value: []byte{6},
				},
			},
		},
		{
			NsId:      "varying number of items",
			NsVersion: 6,
			ReadsOnly: []*protoblocktx.Read{
				{
					Key:     []byte{1, 2},
					Version: types.Version(3),
				},
			},
			ReadWrites: []*protoblocktx.ReadWrite{
				{
					Key:     []byte{1},
					Version: types.Version(2),
					Value:   []byte{3},
				},
				{
					Key:     []byte{4},
					Version: types.Version(5),
					Value:   []byte{6},
				},
				{
					Key:     []byte{7},
					Version: types.Version(8),
					Value:   []byte{9},
				},
			},
			BlindWrites: []*protoblocktx.Write{
				{
					Key:   []byte{10},
					Value: []byte{12},
				},
				{
					Key:   []byte{13},
					Value: []byte{14},
				},
			},
		},
		{
			NsId:      "varying length",
			NsVersion: 6,
			ReadsOnly: []*protoblocktx.Read{
				{Key: make([]byte, 127)},
				{Key: make([]byte, 128)},
				{Key: make([]byte, 129)},
				{Key: make([]byte, 255)},
				{Key: make([]byte, 256)},
				{Key: make([]byte, 257)},
				{Key: make([]byte, 0xffff)},
				{Key: make([]byte, 0x10000)},
				{Key: make([]byte, 0x10001)},
				{Key: make([]byte, 0x100000)},
			},
		},
	},
}
