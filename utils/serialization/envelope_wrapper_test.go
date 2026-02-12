/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/
package serialization_test

import (
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-x-common/protoutil"

	"github.com/hyperledger/fabric-x-committer/utils/serialization"
)

var unwrappers = []struct {
	name   string
	unwrap func([]byte) (*serialization.EnvelopeLite, error)
}{
	{"UnwrapEnvelope", func(b []byte) (*serialization.EnvelopeLite, error) {
		data, hdr, err := serialization.UnwrapEnvelope(b)
		if err != nil {
			return nil, err
		}
		return &serialization.EnvelopeLite{HeaderType: hdr.Type, TxID: hdr.TxId, Data: data}, nil
	}},
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
				_, err := uw.unwrap(nil)
				require.Error(t, err)
			})

			t.Run("Empty envelope", func(t *testing.T) {
				t.Parallel()
				_, err := uw.unwrap([]byte{})
				require.Error(t, err)
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
				_, err := uw.unwrap(input)
				require.Error(t, err)
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
				_, err := uw.unwrap(input)
				require.Error(t, err)
			})
		})
	}
}

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

		origData, origHdr, origErr := serialization.UnwrapEnvelope(wrappedEnvelope)
		liteHdr, liteErr := serialization.UnwrapEnvelopeLite(wrappedEnvelope)

		require.NoError(t, origErr)
		require.NoError(t, liteErr)
		require.Equal(t, origHdr.Type, liteHdr.HeaderType)
		require.Equal(t, origHdr.TxId, liteHdr.TxID)
		require.Equal(t, origData, liteHdr.Data)
	})
}

func BenchmarkUnwrapEnvelope(b *testing.B) {
	envelope := makeTestEnvelope()
	b.ResetTimer()
	for b.Loop() {
		_, _, err := serialization.UnwrapEnvelope(envelope)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkUnwrapEnvelopeLite(b *testing.B) {
	envelope := makeTestEnvelope()
	b.ResetTimer()
	for b.Loop() {
		_, err := serialization.UnwrapEnvelopeLite(envelope)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func makeTestEnvelope() []byte {
	return protoutil.MarshalOrPanic(&common.Envelope{
		Payload: protoutil.MarshalOrPanic(&common.Payload{
			Header: &common.Header{
				ChannelHeader: protoutil.MarshalOrPanic(&common.ChannelHeader{
					Type:      int32(common.HeaderType_MESSAGE),
					TxId:      "benchmark-tx-id-12345",
					ChannelId: "benchmark-channel",
				}),
			},
			Data: []byte("benchmark payload data"),
		}),
	})
}
