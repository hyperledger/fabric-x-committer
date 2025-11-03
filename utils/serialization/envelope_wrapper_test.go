/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/
package serialization_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-x-common/protoutil"

	"github.com/hyperledger/fabric-x-committer/utils/serialization"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

// TestUnwrapEnvelopeBadInput tests UnwrapEnvelope function with invalid inputs.
func TestUnwrapEnvelopeBadInput(t *testing.T) {
	t.Parallel()
	t.Run("Not an envelope", func(t *testing.T) {
		t.Parallel()
		_, _, err := serialization.UnwrapEnvelope([]byte("invalid input"))
		require.Error(t, err)
	})

	t.Run("OK Header with an invalid payload", func(t *testing.T) {
		t.Parallel()
		envelopeBytes := protoutil.MarshalOrPanic(&common.Envelope{
			Payload: []byte("not-a-payload"),
		})
		_, _, err := serialization.UnwrapEnvelope(envelopeBytes)
		require.Error(t, err)
	})

	t.Run("OK Payload with a nil Header", func(t *testing.T) {
		t.Parallel()
		payloadBytes := protoutil.MarshalOrPanic(&common.Payload{
			Header: nil,
			Data:   []byte("some data"),
		})

		envelopeBytes := protoutil.MarshalOrPanic(&common.Envelope{
			Payload: payloadBytes,
		})

		_, _, err := serialization.UnwrapEnvelope(envelopeBytes)
		require.Error(t, err)
	})

	t.Run("OK payload but invalid ChannelHeader", func(t *testing.T) {
		t.Parallel()
		envelopeBytes := protoutil.MarshalOrPanic(&common.Envelope{
			Payload: protoutil.MarshalOrPanic(&common.Payload{
				Header: &common.Header{
					ChannelHeader: []byte("not-a-channel-header"),
				},
				Data: []byte("some data"),
			}),
		})

		_, _, err := serialization.UnwrapEnvelope(envelopeBytes)
		require.Error(t, err)
	})
}

// TestUnwrapEnvelopeGoodInput Tests properly wrapped envelope is unwrapped correctly.
func TestUnwrapEnvelopeGoodInput(t *testing.T) {
	t.Parallel()
	originalPayload := []byte("test payload")

	originalChannelHeader := &common.ChannelHeader{
		ChannelId: "test-channel",
	}

	originalHeader := &common.Header{
		ChannelHeader: protoutil.MarshalOrPanic(originalChannelHeader),
	}

	// Wrap
	wrappedEnvelope := protoutil.MarshalOrPanic(&common.Envelope{
		Payload: protoutil.MarshalOrPanic(&common.Payload{
			Header: originalHeader,
			Data:   originalPayload,
		}),
	})

	// Unwrap
	payload, channelHeader, err := serialization.UnwrapEnvelope(wrappedEnvelope)

	// -Check 1- Check unwrap envelope has no error
	require.NoError(t, err)

	// -Check 2- Check we get the correct Payload & Header
	require.Equal(t, originalPayload, payload)
	test.RequireProtoEqual(t, originalChannelHeader, channelHeader)
}
