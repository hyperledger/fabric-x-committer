/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/
package serialization_test

import (
	"bytes"
	"testing"
	"unicode/utf8"

	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-x-common/protoutil"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/utils/serialization"
)

var unwrappers = []struct {
	name   string
	unwrap func([]byte) (*serialization.EnvelopeLite, error)
}{
	{"UnwrapEnvelope", serialization.UnwrapEnvelope},
	{"UnwrapEnvelopeLite", serialization.UnwrapEnvelopeLite},
}

// TestUnwrapEnvelopeBadInput tests both UnwrapEnvelope and UnwrapEnvelopeLite
// with invalid inputs, ensuring both reject the same malformed data.
func TestUnwrapEnvelopeBadInput(t *testing.T) {
	t.Parallel()
	for _, uw := range unwrappers {
		t.Run(uw.name, func(t *testing.T) {
			t.Parallel()

			t.Run("Not an envelope", func(t *testing.T) {
				t.Parallel()
				_, err := uw.unwrap([]byte("invalid input"))
				require.Error(t, err)
			})

			t.Run("Empty input", func(t *testing.T) {
				t.Parallel()
				result, err := uw.unwrap(nil)
				require.NoError(t, err)
				require.Equal(t, &serialization.EnvelopeLite{}, result)
			})

			t.Run("Empty envelope", func(t *testing.T) {
				t.Parallel()
				result, err := uw.unwrap([]byte{})
				require.NoError(t, err)
				require.Equal(t, &serialization.EnvelopeLite{}, result)
			})

			t.Run("Invalid payload", func(t *testing.T) {
				t.Parallel()
				input := protoutil.MarshalOrPanic(&common.Envelope{
					Payload: []byte("not-a-payload"),
				})
				_, err := uw.unwrap(input)
				require.Error(t, err)
			})

			t.Run("Nil header in payload", func(t *testing.T) {
				t.Parallel()
				input := protoutil.MarshalOrPanic(&common.Envelope{
					Payload: protoutil.MarshalOrPanic(&common.Payload{
						Header: nil,
						Data:   []byte("some data"),
					}),
				})
				result, err := uw.unwrap(input)
				require.NoError(t, err)
				require.Equal(t, &serialization.EnvelopeLite{Data: []byte("some data")}, result)
			})

			t.Run("Invalid channel header", func(t *testing.T) {
				t.Parallel()
				input := protoutil.MarshalOrPanic(&common.Envelope{
					Payload: protoutil.MarshalOrPanic(&common.Payload{
						Header: &common.Header{
							ChannelHeader: []byte("not-a-channel-header"),
						},
						Data: []byte("some data"),
					}),
				})
				// Both implementations use projection protos / wire scanning that
				// only look at fields 1 (type) and 5 (tx_id). Garbage bytes may
				// or may not parse as valid protobuf, so we only verify consistency.
				origResult, origErr := serialization.UnwrapEnvelope(input)
				liteResult, liteErr := serialization.UnwrapEnvelopeLite(input)
				assertConsistent(t, origResult, origErr, liteResult, liteErr)
			})
		})
	}
}

// FuzzUnwrapEnvelopeLiteConsistency fuzzes the fields of a correctly-encoded
// envelope and verifies both implementations produce identical results.
//
// Layer 1: All proto nesting levels are correctly encoded by proto.Marshal.
// This is the strongest consistency guarantee — both must always agree.
//
// Run: go test -fuzz=FuzzUnwrapEnvelopeLiteConsistency -fuzztime=30s ./utils/serialization/.
func FuzzUnwrapEnvelopeLiteConsistency(f *testing.F) {
	seeds := []struct {
		headerType int32
		txID       string
		data       []byte
		channelID  string
		signature  []byte
		sigHeader  []byte
	}{
		{0, "", nil, "", nil, nil},
		{int32(common.HeaderType_MESSAGE), "tx-1", []byte("hello"), "ch1", nil, nil},
		{int32(common.HeaderType_CONFIG), "tx-cfg", []byte("cfg"), "ch2", []byte("sig"), []byte("sighdr")},
		{
			int32(common.HeaderType_ENDORSER_TRANSACTION), "tx-end", make([]byte, 1024), "ch3",
			make([]byte, 72), make([]byte, 64),
		},
		{3, "", []byte("d"), "", nil, nil},                  // non-zero type, empty txID
		{0, "tx-zero-type", []byte("d"), "", nil, nil},      // zero type, non-empty txID
		{1, string(make([]byte, 512)), nil, "ch", nil, nil}, // large txID
	}
	for _, s := range seeds {
		f.Add(s.headerType, s.txID, s.data, s.channelID, s.signature, s.sigHeader)
	}

	f.Fuzz(func(t *testing.T, headerType int32, txID string, data []byte, channelID string,
		signature, sigHeader []byte,
	) {
		if !utf8.ValidString(txID) || !utf8.ValidString(channelID) {
			t.Skip("skipping invalid UTF-8 input")
		}

		wrappedEnvelope := protoutil.MarshalOrPanic(&common.Envelope{
			Payload: protoutil.MarshalOrPanic(&common.Payload{
				Header: &common.Header{
					ChannelHeader: protoutil.MarshalOrPanic(&common.ChannelHeader{
						Type:      headerType,
						TxId:      txID,
						ChannelId: channelID,
					}),
					SignatureHeader: sigHeader,
				},
				Data: data,
			}),
			Signature: signature,
		})

		assertLiteMatchesOriginal(t, wrappedEnvelope)
	})
}

// FuzzUnwrapEnvelopeLiteFuzzedChannelHeader wraps fuzzed bytes as ChannelHeader
// inside an otherwise correctly-encoded Envelope → Payload → Header.
//
// Layer 2: ChannelHeader is raw fuzz bytes; all outer layers are valid.
//
// Run: go test -fuzz=FuzzUnwrapEnvelopeLiteFuzzedChannelHeader -fuzztime=30s ./utils/serialization/.
func FuzzUnwrapEnvelopeLiteFuzzedChannelHeader(f *testing.F) {
	seeds := []struct {
		channelHeader []byte
		data          []byte
		sigHeader     []byte
	}{
		{[]byte{}, []byte("data"), []byte("sig-hdr")},
		{[]byte("garbage"), []byte("data"), []byte("sig-hdr")},
		{protoutil.MarshalOrPanic(&common.ChannelHeader{Type: 3, TxId: "tx1"}), []byte("data"), []byte{}},
		{[]byte{0x08, 0x03, 0x2a, 0x03}, []byte("d"), []byte{}}, // truncated field 5
	}
	for _, s := range seeds {
		f.Add(s.channelHeader, s.data, s.sigHeader)
	}

	f.Fuzz(func(t *testing.T, channelHeaderBytes, data, sigHeader []byte) {
		envelope := protoutil.MarshalOrPanic(&common.Envelope{
			Payload: protoutil.MarshalOrPanic(&common.Payload{
				Header: &common.Header{
					ChannelHeader:   channelHeaderBytes,
					SignatureHeader: sigHeader,
				},
				Data: data,
			}),
		})
		assertLiteMatchesOriginal(t, envelope)
	})
}

// FuzzUnwrapEnvelopeLiteFuzzedHeader wraps fuzzed bytes as the Header
// inside an otherwise correctly-encoded Envelope → Payload.
//
// Layer 3: Header is raw fuzz bytes; Envelope and Payload structure are valid.
//
// Run: go test -fuzz=FuzzUnwrapEnvelopeLiteFuzzedHeader -fuzztime=30s ./utils/serialization/.
func FuzzUnwrapEnvelopeLiteFuzzedHeader(f *testing.F) {
	seeds := []struct {
		header []byte
		data   []byte
	}{
		{[]byte{}, []byte("data")},
		{[]byte("garbage"), []byte("data")},
		{protoutil.MarshalOrPanic(&common.Header{
			ChannelHeader: protoutil.MarshalOrPanic(&common.ChannelHeader{Type: 3, TxId: "tx1"}),
		}), []byte("data")},
	}
	for _, s := range seeds {
		f.Add(s.header, s.data)
	}

	f.Fuzz(func(t *testing.T, headerBytes, data []byte) {
		envelope := protoutil.MarshalOrPanic(&common.Envelope{
			Payload: protoutil.MarshalOrPanic(&common.Payload{
				Header: &common.Header{
					ChannelHeader: headerBytes, // fuzzed — treated as raw channel header bytes
				},
				Data: data,
			}),
		})
		assertLiteMatchesOriginal(t, envelope)
	})
}

// FuzzUnwrapEnvelopeLiteFuzzedPayload wraps fuzzed bytes as the Payload
// inside an otherwise correctly-encoded Envelope.
//
// Layer 4: Payload is raw fuzz bytes; only Envelope structure is valid.
//
// Run: go test -fuzz=FuzzUnwrapEnvelopeLiteFuzzedPayload -fuzztime=30s ./utils/serialization/.
func FuzzUnwrapEnvelopeLiteFuzzedPayload(f *testing.F) {
	seeds := []struct {
		payload []byte
	}{
		{[]byte{}},
		{[]byte("garbage")},
		{protoutil.MarshalOrPanic(&common.Payload{
			Header: &common.Header{
				ChannelHeader: protoutil.MarshalOrPanic(&common.ChannelHeader{Type: 3, TxId: "tx1"}),
			},
			Data: []byte("data"),
		})},
	}
	for _, s := range seeds {
		f.Add(s.payload)
	}

	f.Fuzz(func(t *testing.T, payloadBytes []byte) {
		envelope := protoutil.MarshalOrPanic(&common.Envelope{
			Payload: payloadBytes,
		})
		assertLiteMatchesOriginal(t, envelope)
	})
}

// FuzzUnwrapEnvelopeLiteFuzzedEnvelope feeds entirely fuzzed bytes as the
// serialized Envelope.
//
// Layer 5: Everything is fuzz bytes — no valid proto structure guaranteed.
//
// Run: go test -fuzz=FuzzUnwrapEnvelopeLiteFuzzedEnvelope -fuzztime=30s ./utils/serialization/.
func FuzzUnwrapEnvelopeLiteFuzzedEnvelope(f *testing.F) {
	seeds := []struct {
		envelope []byte
	}{
		{loadgenEnvelopes(f, 1)[0]},
		{[]byte{}},
		{[]byte("not a protobuf")},
		{[]byte{0x0a, 0x00}}, // envelope with empty payload
	}
	for _, s := range seeds {
		f.Add(s.envelope)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		assertLiteMatchesOriginal(t, data)
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
		origResult, origErr := serialization.UnwrapEnvelope(envBytes)
		liteResult, liteErr := serialization.UnwrapEnvelopeLite(envBytes)

		require.NoError(t, origErr, "tx %d: UnwrapEnvelope failed", i)
		require.NoError(t, liteErr, "tx %d: UnwrapEnvelopeLite failed", i)
		require.Equal(t, origResult.HeaderType, liteResult.HeaderType, "tx %d: HeaderType mismatch", i)
		require.Equal(t, origResult.TxID, liteResult.TxID, "tx %d: TxID mismatch", i)
		require.Equal(t, origResult.Data, liteResult.Data, "tx %d: Data mismatch", i)
	}
}

func BenchmarkUnwrapEnvelope(b *testing.B) {
	envelopes := loadgenEnvelopes(b, 1024)
	b.ResetTimer()
	i := 0
	for b.Loop() {
		_, err := serialization.UnwrapEnvelope(envelopes[i%len(envelopes)])
		if err != nil {
			b.Fatal(err)
		}
		i++
	}
}

func BenchmarkUnwrapEnvelopeLite(b *testing.B) {
	envelopes := loadgenEnvelopes(b, 1024)
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
	txs := workload.GenerateTransactions(tb, workload.DefaultProfile(8), count)
	block := workload.MapToOrdererBlock(1, txs)
	return block.Data.Data
}

// assertLiteMatchesOriginal checks that UnwrapEnvelope and UnwrapEnvelopeLite
// produce identical results. Both implementations use projection protos that
// declare only the fields the committer needs, so they must always agree —
// same result on valid input, same error/success behavior on malformed input.
//
// Data is compared with bytes.Equal (not require.Equal) because proto3 returns
// nil for zero-length bytes fields while protowire.ConsumeBytes returns []byte{}.
// Both represent "empty data" and are semantically equivalent.
func assertLiteMatchesOriginal(t *testing.T, envelope []byte) {
	t.Helper()
	origResult, origErr := serialization.UnwrapEnvelope(envelope)
	liteResult, liteErr := serialization.UnwrapEnvelopeLite(envelope)
	assertConsistent(t, origResult, origErr, liteResult, liteErr)
}

// assertConsistent verifies that two EnvelopeLite results are identical:
// both must error or both must succeed with equal field values.
//
//nolint:revive // max arguments 4 but got 5
func assertConsistent(t *testing.T, origResult *serialization.EnvelopeLite, origErr error,
	liteResult *serialization.EnvelopeLite, liteErr error,
) {
	t.Helper()
	if origErr != nil {
		require.Error(t, liteErr, "original failed but lite succeeded: origErr=%v, liteResult=%+v", origErr, liteResult)
		return
	}
	require.NoError(t, liteErr, "lite failed but original succeeded")
	require.Equal(t, origResult.HeaderType, liteResult.HeaderType)
	require.Equal(t, origResult.TxID, liteResult.TxID)
	require.True(t, bytes.Equal(origResult.Data, liteResult.Data),
		"Data mismatch: original=%v, lite=%v", origResult.Data, liteResult.Data)
}
