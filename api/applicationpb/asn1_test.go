/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package applicationpb

import (
	"math/rand"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/utils"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

func TestAsnMarshal(t *testing.T) {
	t.Parallel()
	CommonTestAsnMarshal(t, []*TestTx{{
		ID:         "some-tx-id",
		Namespaces: txTestCases,
	}})
}

func FuzzASN1MarshalTxNamespace(f *testing.F) {
	i := uint32(1024)
	for _, ns := range txTestCases {
		//nolint:gosec // false positive; safe integer conversion.
		for _, r := range ns.ReadsOnly {
			f.Add(
				"some-tx-id", ns.NsId, ns.NsVersion, protoToAsnVersion(r.Version),
				uint32(len(ns.ReadsOnly)), uint32(len(ns.ReadWrites)), uint32(len(ns.BlindWrites)),
				i, int64(1234+i),
			)
			i++
		}
		//nolint:gosec // false positive; safe integer conversion.
		for _, r := range ns.ReadWrites {
			f.Add(
				"another-tx-id", ns.NsId, ns.NsVersion, protoToAsnVersion(r.Version),
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
		txID, txNs := generateTxNs(t, id, nsID, nsVersion, readVersion, rCount, rwCount, wCount, maxSize, seed)
		derBytes := requireASN1Marshal(t, txID, txNs)
		actualTxID, actualTxNs := reconstructTX(t, [][]byte{derBytes})
		require.Equal(t, txID, actualTxID)
		test.RequireProtoElementsMatch(t, []*TxNamespace{txNs}, actualTxNs)
	})
}

// generateTxNs creates a TX with a single namespace given the input parameters.
func generateTxNs( //nolint:revive // required parameters.
	t *testing.T,
	id, nsID string, nsVersion uint64, readVersion int64,
	rCount, rwCount, wCount uint32,
	maxSize uint32, seed int64,
) (txID string, tx *TxNamespace) {
	t.Helper()
	if !utf8.ValidString(id) || !utf8.ValidString(nsID) {
		t.Skip("invalid UTF8")
	}
	tx = &TxNamespace{
		NsId:        nsID,
		NsVersion:   nsVersion,
		ReadsOnly:   make([]*Read, rCount),
		ReadWrites:  make([]*ReadWrite, rwCount),
		BlindWrites: make([]*Write, wCount),
	}
	rnd := rand.New(rand.NewSource(seed))
	for i := range tx.ReadsOnly {
		tx.ReadsOnly[i] = &Read{
			Key:     utils.MustRead(rnd, rnd.Intn(int(maxSize))),
			Version: asnToProtoVersion(readVersion),
		}
	}
	for i := range tx.ReadWrites {
		tx.ReadWrites[i] = &ReadWrite{
			Key:     utils.MustRead(rnd, rnd.Intn(int(maxSize))),
			Value:   utils.MustRead(rnd, rnd.Intn(int(maxSize))),
			Version: asnToProtoVersion(readVersion),
		}
	}
	for i := range tx.BlindWrites {
		tx.BlindWrites[i] = &Write{
			Key:   utils.MustRead(rnd, rnd.Intn(int(maxSize))),
			Value: utils.MustRead(rnd, rnd.Intn(int(maxSize))),
		}
	}
	return id, tx
}

var txTestCases = []*TxNamespace{
	{
		NsId:      "empty",
		NsVersion: 1,
	},
	{
		NsId:      "only reads",
		NsVersion: 2,
		ReadsOnly: []*Read{
			{
				Key:     []byte{1},
				Version: NewVersion(2),
			},
			{
				Key:     []byte{3, 4, 5},
				Version: NewVersion(0),
			},
		},
	},
	{
		NsId:      "only reads with nil version",
		NsVersion: 2,
		ReadsOnly: []*Read{
			{
				Key:     []byte{1},
				Version: NewVersion(2),
			},
			{
				Key:     []byte{3, 4, 5},
				Version: NewVersion(0),
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
		ReadWrites: []*ReadWrite{
			{
				Key:     []byte{1},
				Version: NewVersion(2),
				Value:   []byte{3},
			},
			{
				Key:     []byte{5},
				Version: NewVersion(0),
				Value:   []byte{6},
			},
		},
	},
	{
		NsId:      "only read-write with nil value or version",
		NsVersion: 3,
		ReadWrites: []*ReadWrite{
			{
				Key:     []byte{1},
				Version: NewVersion(2),
				Value:   []byte{3},
			},
			{
				Key:     []byte{7},
				Version: nil,
				Value:   []byte{8},
			},
			{
				Key:     []byte{9},
				Version: NewVersion(3),
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
		BlindWrites: []*Write{
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
		BlindWrites: []*Write{
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
		ReadsOnly: []*Read{
			{
				Key:     []byte{6},
				Version: NewVersion(7),
			},
			{
				Key:     []byte{9, 10, 11},
				Version: NewVersion(12),
			},
		},
		ReadWrites: []*ReadWrite{
			{
				Key:     []byte{100},
				Version: NewVersion(1),
				Value:   []byte{2},
			},
			{
				Key:     []byte{5},
				Version: NewVersion(10),
				Value:   []byte{13},
			},
		},
		BlindWrites: []*Write{
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
		ReadsOnly: []*Read{
			{
				Key:     []byte{1, 2},
				Version: NewVersion(3),
			},
		},
		ReadWrites: []*ReadWrite{
			{
				Key:     []byte{1},
				Version: NewVersion(2),
				Value:   []byte{3},
			},
			{
				Key:     []byte{4},
				Version: NewVersion(5),
				Value:   []byte{6},
			},
			{
				Key:     []byte{7},
				Version: NewVersion(8),
				Value:   []byte{9},
			},
		},
		BlindWrites: []*Write{
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
		ReadsOnly: []*Read{
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
