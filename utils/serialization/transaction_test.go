/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/
package serialization_test

import (
	"testing"

	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-committer/utils/serialization"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

func TestUnmarshalTx(t *testing.T) {
	t.Parallel()

	t.Run("into a fresh TX matches UnmarshalTx", func(t *testing.T) {
		t.Parallel()
		want := txForTest("ns-0", "k0")
		data, err := proto.Marshal(want)
		require.NoError(t, err)

		byValue, err := serialization.UnmarshalTx(data)
		require.NoError(t, err)

		var into applicationpb.Tx
		require.NoError(t, serialization.UnmarshalTxInto(data, &into))

		test.RequireProtoEqual(t, want, byValue)
		test.RequireProtoEqual(t, want, &into)
	})

	// The usage this exists for: one backing array for a whole block's transactions, with each
	// decoded through its own address. Nothing may copy an element out of it.
	t.Run("into a slab of TXs", func(t *testing.T) {
		t.Parallel()
		const count = 8
		want := make([]*applicationpb.Tx, count)
		encoded := make([][]byte, count)
		for i := range count {
			want[i] = txForTest("ns-"+string(rune('a'+i)), "key-"+string(rune('a'+i)))
			data, err := proto.Marshal(want[i])
			require.NoError(t, err)
			encoded[i] = data
		}

		slab := make([]applicationpb.Tx, count)
		for i := range count {
			require.NoError(t, serialization.UnmarshalTxInto(encoded[i], &slab[i]))
		}
		for i := range count {
			test.RequireProtoEqual(t, want[i], &slab[i])
		}
	})

	// Pins the behaviour the slab depends on. proto.Unmarshal resets its destination first, so a
	// TX carrying earlier content is replaced rather than merged into; merging is the opt-in
	// behaviour of UnmarshalOptions.Merge. Were it otherwise, every reused entry would silently
	// accumulate the previous transaction's namespaces.
	t.Run("a non-zero destination is replaced, not merged", func(t *testing.T) {
		t.Parallel()
		first := txForTest("ns-first", "k-first")
		second := txForTest("ns-second", "k-second")
		secondData, err := proto.Marshal(second)
		require.NoError(t, err)

		reused := proto.CloneOf(first)
		require.NoError(t, serialization.UnmarshalTxInto(secondData, reused))
		test.RequireProtoEqual(t, second, reused)
	})

	t.Run("invalid bytes report an error", func(t *testing.T) {
		t.Parallel()
		// Field 1 with wire type 6, which is not a valid protobuf wire type.
		invalid := []byte{0x0e, 0xff}

		var into applicationpb.Tx
		require.ErrorContains(t, serialization.UnmarshalTxInto(invalid, &into), "failed to unmarshal tx")

		tx, err := serialization.UnmarshalTx(invalid)
		require.ErrorContains(t, err, "failed to unmarshal tx")
		require.Nil(t, tx)
	})
}

func txForTest(nsID, key string) *applicationpb.Tx {
	return &applicationpb.Tx{
		Namespaces: []*applicationpb.TxNamespace{{
			NsId:        nsID,
			NsVersion:   0,
			BlindWrites: []*applicationpb.Write{{Key: []byte(key), Value: []byte("v")}},
		}},
	}
}
