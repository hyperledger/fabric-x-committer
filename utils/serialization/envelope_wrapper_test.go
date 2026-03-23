/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/
package serialization_test

import (
	"testing"
	"unicode/utf8"

	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/hyperledger/fabric-x-common/protoutil"

	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/utils/serialization"
)

// TestUnwrapEnvelopeLite tests UnwrapEnvelopeLite with well-formed input.
// The orderer validates envelope structure before including transactions
// in a block, so the committer only receives valid envelopes.
func TestUnwrapEnvelopeLite(t *testing.T) {
	t.Parallel()

	t.Run("Empty input", func(t *testing.T) {
		t.Parallel()
		result, err := serialization.UnwrapEnvelopeLite(nil)
		require.NoError(t, err)
		require.Equal(t, &serialization.EnvelopeLite{}, result)
	})

	t.Run("Empty envelope", func(t *testing.T) {
		t.Parallel()
		result, err := serialization.UnwrapEnvelopeLite([]byte{})
		require.NoError(t, err)
		require.Equal(t, &serialization.EnvelopeLite{}, result)
	})

	t.Run("Nil header in payload", func(t *testing.T) {
		t.Parallel()
		input := protoutil.MarshalOrPanic(&common.Envelope{
			Payload: protoutil.MarshalOrPanic(&common.Payload{
				Header: nil,
				Data:   []byte("some data"),
			}),
		})
		result, err := serialization.UnwrapEnvelopeLite(input)
		require.NoError(t, err)
		require.Equal(t, &serialization.EnvelopeLite{Data: []byte("some data")}, result)
	})

	t.Run("Invalid UTF-8 in tx_id", func(t *testing.T) {
		t.Parallel()
		// Hand-craft a ChannelHeader with invalid UTF-8 in the tx_id field (field 5).
		// Field 5, wire type 2 (bytes): tag = 5<<3|2 = 0x2a, length = 2, value = [0xff, 0xfe].
		invalidUTF8ChanHdr := []byte{0x2a, 0x02, 0xff, 0xfe}
		input := protoutil.MarshalOrPanic(&common.Envelope{
			Payload: protoutil.MarshalOrPanic(&common.Payload{
				Header: &common.Header{
					ChannelHeader: invalidUTF8ChanHdr,
				},
				Data: []byte("some data"),
			}),
		})
		_, err := serialization.UnwrapEnvelopeLite(input)
		require.Error(t, err)
	})
}

// FuzzUnwrapEnvelopeLiteConsistency fuzzes the fields of a correctly-encoded
// envelope and verifies UnwrapEnvelopeLite produces results identical to
// UnwrapEnvelope. All proto nesting levels are correctly encoded by
// proto.Marshal, matching the real-world path where the orderer validates
// envelope structure.
//
// Run: go test -fuzz=FuzzUnwrapEnvelopeLiteConsistency -fuzztime=30s ./utils/serialization/.
func FuzzUnwrapEnvelopeLiteConsistency(f *testing.F) {
	seeds := []struct {
		headerType  int32
		txID        string
		data        []byte
		channelID   string
		signature   []byte
		version     int32
		tsSeconds   int64
		tsNanos     int32
		epoch       uint64
		extension   []byte
		tlsCertHash []byte
		creator     []byte
		nonce       []byte
	}{
		{0, "", nil, "", nil, 0, 0, 0, 0, nil, nil, nil, nil},
		{
			int32(common.HeaderType_MESSAGE), "tx-1", []byte("hello"), "ch1", nil,
			1, 1710000000, 123456789, 0, nil, nil, []byte("creator1"), []byte("nonce1"),
		},
		{
			int32(common.HeaderType_CONFIG), "tx-cfg", []byte("cfg"), "ch2", []byte("sig"),
			2, 1710000001, 0, 1, []byte("ext-cfg"), []byte("tls-hash"), []byte("creator2"), []byte("nonce2"),
		},
		{
			int32(common.HeaderType_ENDORSER_TRANSACTION), "tx-end", make([]byte, 1024), "ch3",
			make([]byte, 72),
			1, 1710000002, 999999999, 42, make([]byte, 256), make([]byte, 32),
			make([]byte, 128), make([]byte, 24),
		},
		{3, "", []byte("d"), "", nil, 0, 0, 0, 0, nil, nil, nil, nil},
		{0, "tx-zero-type", []byte("d"), "", nil, 0, 0, 0, 0, nil, nil, nil, nil},
		{1, string(make([]byte, 512)), nil, "ch", nil, 0, 0, 0, 0, nil, nil, nil, nil},
	}
	for _, s := range seeds {
		f.Add(s.headerType, s.txID, s.data, s.channelID, s.signature,
			s.version, s.tsSeconds, s.tsNanos, s.epoch, s.extension, s.tlsCertHash,
			s.creator, s.nonce)
	}

	f.Fuzz(func(t *testing.T, headerType int32, txID string, data []byte, channelID string,
		signature []byte, version int32, tsSeconds int64, tsNanos int32, epoch uint64,
		extension, tlsCertHash, creator, nonce []byte,
	) {
		if !utf8.ValidString(txID) || !utf8.ValidString(channelID) {
			t.Skip("proto.Marshal panics on invalid UTF-8 strings")
		}

		wrappedEnvelope := protoutil.MarshalOrPanic(&common.Envelope{
			Payload: protoutil.MarshalOrPanic(&common.Payload{
				Header: &common.Header{
					ChannelHeader: protoutil.MarshalOrPanic(&common.ChannelHeader{
						Type:        headerType,
						Version:     version,
						Timestamp:   &timestamppb.Timestamp{Seconds: tsSeconds, Nanos: tsNanos},
						ChannelId:   channelID,
						TxId:        txID,
						Epoch:       epoch,
						Extension:   extension,
						TlsCertHash: tlsCertHash,
					}),
					SignatureHeader: protoutil.MarshalOrPanic(&common.SignatureHeader{
						Creator: creator,
						Nonce:   nonce,
					}),
				},
				Data: data,
			}),
			Signature: signature,
		})

		assertLiteMatchesOriginal(t, wrappedEnvelope)
	})
}

// FuzzUnwrapEnvelopeLiteNoPanic feeds fully fuzzed bytes and verifies
// UnwrapEnvelopeLite does not panic on arbitrary input.
//
// Run: go test -fuzz=FuzzUnwrapEnvelopeLiteNoPanic -fuzztime=30s ./utils/serialization/.
func FuzzUnwrapEnvelopeLiteNoPanic(f *testing.F) {
	f.Add(loadgenEnvelopes(f, 1)[0])
	f.Add([]byte{})
	f.Add([]byte("not a protobuf"))
	f.Add([]byte{0x0a, 0x00})

	f.Fuzz(func(_ *testing.T, data []byte) {
		_, _ = serialization.UnwrapEnvelopeLite(data) // checking for panics only
	})
}

// TestLoadgenEnvelopeConsistency generates 1000 realistic transactions using
// the load generator and verifies that both UnwrapEnvelope and UnwrapEnvelopeLite
// produce identical results for every transaction.
func TestLoadgenEnvelopeConsistency(t *testing.T) {
	t.Parallel()
	const txCount = 1000
	txs := workload.GenerateTransactions(t, workload.DefaultProfile(8), txCount)
	block := workload.MapToOrdererBlock(1, txs)

	for i, envBytes := range block.Data.Data {
		origData, origHdr, origErr := serialization.UnwrapEnvelope(envBytes)
		liteResult, liteErr := serialization.UnwrapEnvelopeLite(envBytes)

		require.NoError(t, origErr, "tx %d: UnwrapEnvelope failed", i)
		require.NoError(t, liteErr, "tx %d: UnwrapEnvelopeLite failed", i)
		require.Equal(t, origHdr.Type, liteResult.HeaderType, "tx %d: HeaderType mismatch", i)
		require.Equal(t, origHdr.TxId, liteResult.TxID, "tx %d: TxID mismatch", i)
		require.Equal(t, origData, liteResult.Data, "tx %d: Data mismatch", i)
	}
}

func BenchmarkUnwrapEnvelope(b *testing.B) {
	envelopes := loadgenEnvelopes(b, b.N)
	b.ResetTimer()
	i := 0
	for b.Loop() {
		_, _, err := serialization.UnwrapEnvelope(envelopes[i%len(envelopes)])
		if err != nil {
			b.Fatal(err)
		}
		i++
	}
}

func BenchmarkUnwrapEnvelopeLite(b *testing.B) {
	envelopes := loadgenEnvelopes(b, b.N)
	b.ResetTimer()
	i := 0
	for b.Loop() {
		_, err := serialization.UnwrapEnvelopeLite(envelopes[i%len(envelopes)])
		if err != nil {
			b.Fatal(err)
		}
		i++
	}
}

// loadgenEnvelopes generates realistic serialized envelopes using the load
// generator, matching how the sidecar benchmark produces test data.
func loadgenEnvelopes(tb testing.TB, count int) [][]byte {
	tb.Helper()
	txs := workload.GenerateTransactions(tb, nil, count)
	block := workload.MapToOrdererBlock(1, txs)
	return block.Data.Data
}

// assertLiteMatchesOriginal checks that when UnwrapEnvelope succeeds,
// UnwrapEnvelopeLite produces the same result (same values).
func assertLiteMatchesOriginal(t *testing.T, envelope []byte) {
	t.Helper()
	origData, origHdr, origErr := serialization.UnwrapEnvelope(envelope)
	liteResult, liteErr := serialization.UnwrapEnvelopeLite(envelope)

	if origErr != nil {
		return // relaxed: if protobuf rejects it, lite can do anything
	}
	require.NoError(t, liteErr, "lite failed but original succeeded")
	if origHdr != nil {
		require.Equal(t, origHdr.Type, liteResult.HeaderType)
		require.Equal(t, origHdr.TxId, liteResult.TxID)
	}
	require.Equal(t, origData, liteResult.Data,
		"Data mismatch: original=%v, lite=%v", origData, liteResult.Data)
}
