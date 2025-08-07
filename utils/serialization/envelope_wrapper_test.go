/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/
package serialization_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric/protoutil"

	"github.com/hyperledger/fabric-x-committer/utils/serialization"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

// TestUnwrapEnvelopeBadInput tests UnwrapEnvelope function with invalid inputs.
func TestUnwrapEnvelopeBadInput(t *testing.T) {
	t.Run("Not an envelope", func(t *testing.T) {
		t.Parallel()
		_, _, err := serialization.UnwrapEnvelope([]byte("invalid input"))
		require.Error(t, err)
	})

	t.Run("OK Header with an invalid payload", func(t *testing.T) {
		t.Parallel()
		envelope := &common.Envelope{
			Payload: []byte("not-a-payload"),
		}

		envelopeBytes := protoutil.MarshalOrPanic(envelope)

		_, _, err := serialization.UnwrapEnvelope(envelopeBytes)
		require.Error(t, err)
	})

	t.Run("OK Payload with a nil Header", func(t *testing.T) {
		t.Parallel()
		payload := &common.Payload{
			Header: nil,
			Data:   []byte("some data"),
		}
		payloadBytes := protoutil.MarshalOrPanic(payload)

		envelope := &common.Envelope{
			Payload: payloadBytes,
		}
		envelopeBytes := protoutil.MarshalOrPanic(envelope)

		_, _, err := serialization.UnwrapEnvelope(envelopeBytes)
		require.Error(t, err)
	})

	t.Run("OK payload but invalid ChannelHeader", func(t *testing.T) {
		t.Parallel()
		header := &common.Header{
			ChannelHeader: []byte("not-a-channel-header"),
		}
		payload := &common.Payload{
			Header: header,
			Data:   []byte("some data"),
		}
		payloadBytes := protoutil.MarshalOrPanic(payload)

		envelope := &common.Envelope{
			Payload: payloadBytes,
		}
		envelopeBytes := protoutil.MarshalOrPanic(envelope)

		_, _, err := serialization.UnwrapEnvelope(envelopeBytes)
		require.Error(t, err)
	})
}

// TestUnwrapEnvelopeGoodInput Tests properly wrapped envelope is unwrapped correctly.
func TestUnwrapEnvelopeGoodInput(t *testing.T) {
	t.Parallel()
	// -1- Check unwrap envelope has no error
	originalPayload := []byte("test payload")
	originalChannelHeader := &common.ChannelHeader{
		ChannelId: "test-channel",
	}
	originalHeader := &common.Header{
		ChannelHeader: protoutil.MarshalOrPanic(originalChannelHeader),
	}
	wrappedEnvelope := serialization.WrapEnvelope(originalPayload, originalHeader)
	payload, channelHeader, err := serialization.UnwrapEnvelope(wrappedEnvelope)

	// -2- Check we get the correct Payload & Header
	require.NoError(t, err)
	require.Equal(t, originalPayload, payload)
	test.RequireProtoEqual(t, originalChannelHeader, channelHeader)
}
