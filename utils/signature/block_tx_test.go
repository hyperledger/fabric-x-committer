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

	"github.com/hyperledger/fabric-x-committer/api/applicationpb"
	"github.com/hyperledger/fabric-x-committer/api/committerpb"
	"github.com/hyperledger/fabric-x-committer/api/protoloadgen"
	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/utils"
	"github.com/hyperledger/fabric-x-committer/utils/logging"
	"github.com/hyperledger/fabric-x-committer/utils/signature"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

func BenchmarkDigest(b *testing.B) {
	logging.SetupWithConfig(&logging.Config{Enabled: false})
	txs := workload.GenerateTransactions(b, workload.DefaultProfile(8), b.N)

	resBench := make([][]byte, b.N)
	errBench := make([]error, b.N)

	b.ResetTimer()
	for i, tx := range txs {
		resBench[i], errBench[i] = signature.DigestTxNamespace(tx.Id, tx.Tx.Namespaces[0])
	}
	b.StopTimer()
	for i := range b.N {
		require.NoError(b, errBench[i], "error at index %d", i)
		require.NotNil(b, resBench[i], "no result at index %d", i)
	}
}

func TestAsnMarshal(t *testing.T) {
	t.Parallel()

	txs := []*protoloadgen.TX{{
		Id: "some-tx-id",
		Tx: &applicationpb.Tx{Namespaces: txTestCases},
	}}
	// We test against the generated load to enforce a coupling between different parts of the system.
	txs = append(txs, workload.GenerateTransactions(t, workload.DefaultProfile(8), 1024)...)

	for _, tx := range txs {
		tx := tx
		// We don't serialize the signature.
		tx.Tx.Endorsements = nil
		t.Run(tx.Id, func(t *testing.T) {
			t.Parallel()
			for _, ns := range tx.Tx.Namespaces {
				ns := ns
				t.Run(ns.NsId, func(t *testing.T) {
					t.Parallel()
					requireASN1Marshal(t, tx.Id, ns)
				})
			}

			// Test that we can reconstruct the TX from the marshalled version.
			// This ensures we didn't lose any data in the translation.
			t.Run("verify reconstruction of the TX", func(t *testing.T) {
				t.Parallel()
				translatedNamespace := make([][]byte, len(tx.Tx.Namespaces))
				for i, ns := range tx.Tx.Namespaces {
					var err error
					translatedNamespace[i], err = signature.ASN1MarshalTxNamespace(tx.Id, ns)
					require.NoError(t, err)
				}
				txID, actualTx := reconstructTX(t, translatedNamespace)
				require.Equal(t, tx.Id, txID)
				test.RequireProtoEqual(t, tx.Tx, actualTx)
			})
		})
	}
}

func FuzzASN1MarshalTxNamespace(f *testing.F) {
	i := uint32(1024)
	for _, ns := range txTestCases {
		//nolint:gosec // false positive; safe integer conversion.
		for _, r := range ns.ReadsOnly {
			f.Add(
				"some-tx-id", ns.NsId, ns.NsVersion, signature.ProtoToAsnVersion(r.Version),
				uint32(len(ns.ReadsOnly)), uint32(len(ns.ReadWrites)), uint32(len(ns.BlindWrites)),
				i, int64(1234+i),
			)
			i++
		}
		//nolint:gosec // false positive; safe integer conversion.
		for _, r := range ns.ReadWrites {
			f.Add(
				"some-tx-id", ns.NsId, ns.NsVersion, signature.ProtoToAsnVersion(r.Version),
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
		txID, tx := generateTX(t, id, nsID, nsVersion, readVersion, rCount, rwCount, wCount, maxSize, seed)
		derBytes := requireASN1Marshal(t, txID, tx.Namespaces[0])
		actualTxID, actualTx := reconstructTX(t, [][]byte{derBytes})
		require.Equal(t, txID, actualTxID)
		test.RequireProtoEqual(t, tx, actualTx)
	})
}

func requireASN1Marshal(t *testing.T, txID string, ns *applicationpb.TxNamespace) []byte {
	t.Helper()
	translated := signature.TranslateTx(txID, ns)

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

	derBytes, err := signature.ASN1MarshalTxNamespace(txID, ns)
	require.NoError(t, err)

	actual := &signature.TxWithNamespace{}
	_, err = asn1.Unmarshal(derBytes, actual)
	require.NoError(t, err)
	require.Equal(t, translated, actual)
	return derBytes
}

// reconstructTX unmarshal the given namespaces and reconstruct a TX.
// Any change to [*protoblocktx.Tx] requires a change to this method.
func reconstructTX(t *testing.T, namespaces [][]byte) (txID string, tx *applicationpb.Tx) {
	t.Helper()
	tx = &applicationpb.Tx{}

	for i, nsDer := range namespaces {
		translated := &signature.TxWithNamespace{}
		_, err := asn1.Unmarshal(nsDer, translated)
		require.NoError(t, err)

		if i == 0 {
			txID = translated.TxID
		} else {
			require.Equal(t, txID, translated.TxID)
		}

		nsVer := signature.AsnToProtoVersion(translated.NamespaceVersion)
		require.NotNil(t, nsVer)
		ns := &applicationpb.TxNamespace{
			NsId:        translated.NamespaceID,
			NsVersion:   *nsVer,
			ReadsOnly:   make([]*applicationpb.Read, len(translated.ReadsOnly)),
			ReadWrites:  make([]*applicationpb.ReadWrite, len(translated.ReadWrites)),
			BlindWrites: make([]*applicationpb.Write, len(translated.BlindWrites)),
		}
		tx.Namespaces = append(tx.Namespaces, ns)

		for j, read := range translated.ReadsOnly {
			ns.ReadsOnly[j] = &applicationpb.Read{
				Key:     read.Key,
				Version: signature.AsnToProtoVersion(read.Version),
			}
		}
		for j, rw := range translated.ReadWrites {
			ns.ReadWrites[j] = &applicationpb.ReadWrite{
				Key:     rw.Key,
				Value:   rw.Value,
				Version: signature.AsnToProtoVersion(rw.Version),
			}
		}
		for j, w := range translated.BlindWrites {
			ns.BlindWrites[j] = &applicationpb.Write{
				Key:   w.Key,
				Value: w.Value,
			}
		}
	}

	return txID, tx
}

// generateTX creates a TX with a single namespace given the input parameters.
func generateTX( //nolint:revive // required parameters.
	t *testing.T,
	id, nsID string, nsVersion uint64, readVersion int64,
	rCount, rwCount, wCount uint32,
	maxSize uint32, seed int64,
) (txID string, tx *applicationpb.Tx) {
	t.Helper()
	if !utf8.ValidString(id) || !utf8.ValidString(nsID) {
		t.Skip("invalid UTF8")
	}
	tx = &applicationpb.Tx{
		Namespaces: []*applicationpb.TxNamespace{
			{
				NsId:        nsID,
				NsVersion:   nsVersion,
				ReadsOnly:   make([]*applicationpb.Read, rCount),
				ReadWrites:  make([]*applicationpb.ReadWrite, rwCount),
				BlindWrites: make([]*applicationpb.Write, wCount),
			},
		},
	}
	rnd := rand.New(rand.NewSource(seed))
	for i := range tx.Namespaces[0].ReadsOnly {
		tx.Namespaces[0].ReadsOnly[i] = &applicationpb.Read{
			Key:     utils.MustRead(rnd, rnd.Intn(int(maxSize))),
			Version: signature.AsnToProtoVersion(readVersion),
		}
	}
	for i := range tx.Namespaces[0].ReadWrites {
		tx.Namespaces[0].ReadWrites[i] = &applicationpb.ReadWrite{
			Key:     utils.MustRead(rnd, rnd.Intn(int(maxSize))),
			Value:   utils.MustRead(rnd, rnd.Intn(int(maxSize))),
			Version: signature.AsnToProtoVersion(readVersion),
		}
	}
	for i := range tx.Namespaces[0].BlindWrites {
		tx.Namespaces[0].BlindWrites[i] = &applicationpb.Write{
			Key:   utils.MustRead(rnd, rnd.Intn(int(maxSize))),
			Value: utils.MustRead(rnd, rnd.Intn(int(maxSize))),
		}
	}
	return id, tx
}

var txTestCases = []*applicationpb.TxNamespace{
	{
		NsId:      "empty",
		NsVersion: 1,
	},
	{
		NsId:      "only reads",
		NsVersion: 2,
		ReadsOnly: []*applicationpb.Read{
			{
				Key:     []byte{1},
				Version: committerpb.Version(2),
			},
			{
				Key:     []byte{3, 4, 5},
				Version: committerpb.Version(0),
			},
		},
	},
	{
		NsId:      "only reads with nil version",
		NsVersion: 2,
		ReadsOnly: []*applicationpb.Read{
			{
				Key:     []byte{1},
				Version: committerpb.Version(2),
			},
			{
				Key:     []byte{3, 4, 5},
				Version: committerpb.Version(0),
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
		ReadWrites: []*applicationpb.ReadWrite{
			{
				Key:     []byte{1},
				Version: committerpb.Version(2),
				Value:   []byte{3},
			},
			{
				Key:     []byte{5},
				Version: committerpb.Version(0),
				Value:   []byte{6},
			},
		},
	},
	{
		NsId:      "only read-write with nil value or version",
		NsVersion: 3,
		ReadWrites: []*applicationpb.ReadWrite{
			{
				Key:     []byte{1},
				Version: committerpb.Version(2),
				Value:   []byte{3},
			},
			{
				Key:     []byte{7},
				Version: nil,
				Value:   []byte{8},
			},
			{
				Key:     []byte{9},
				Version: committerpb.Version(3),
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
		BlindWrites: []*applicationpb.Write{
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
		BlindWrites: []*applicationpb.Write{
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
		ReadsOnly: []*applicationpb.Read{
			{
				Key:     []byte{6},
				Version: committerpb.Version(7),
			},
			{
				Key:     []byte{9, 10, 11},
				Version: committerpb.Version(12),
			},
		},
		ReadWrites: []*applicationpb.ReadWrite{
			{
				Key:     []byte{100},
				Version: committerpb.Version(1),
				Value:   []byte{2},
			},
			{
				Key:     []byte{5},
				Version: committerpb.Version(10),
				Value:   []byte{13},
			},
		},
		BlindWrites: []*applicationpb.Write{
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
		ReadsOnly: []*applicationpb.Read{
			{
				Key:     []byte{1, 2},
				Version: committerpb.Version(3),
			},
		},
		ReadWrites: []*applicationpb.ReadWrite{
			{
				Key:     []byte{1},
				Version: committerpb.Version(2),
				Value:   []byte{3},
			},
			{
				Key:     []byte{4},
				Version: committerpb.Version(5),
				Value:   []byte{6},
			},
			{
				Key:     []byte{7},
				Version: committerpb.Version(8),
				Value:   []byte{9},
			},
		},
		BlindWrites: []*applicationpb.Write{
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
		ReadsOnly: []*applicationpb.Read{
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
}
