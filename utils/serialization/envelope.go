/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package serialization

import (
	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-x-common/protoutil"
)

// UnwrapEnvelope deserialize an envelope.
func UnwrapEnvelope(message []byte) ([]byte, *common.ChannelHeader, error) {
	envelope, err := protoutil.GetEnvelopeFromBlock(message)
	if err != nil {
		return nil, nil, errors.Wrap(err, "error parsing envelope")
	}

	payload, channelHdr, err := ParseEnvelope(envelope)
	if err != nil {
		return nil, nil, err
	}

	return payload.Data, channelHdr, nil
}

// ParseEnvelope parse the envelope content.
func ParseEnvelope(envelope *common.Envelope) (*common.Payload, *common.ChannelHeader, error) {
	if envelope == nil {
		return nil, nil, errors.New("nil envelope")
	}

	payload, err := protoutil.UnmarshalPayload(envelope.Payload)
	if err != nil {
		return nil, nil, errors.Wrap(err, "error unmarshaling payload")
	}

	if payload.Header == nil { // Will panic if payload.Header is nil
		return nil, nil, errors.New("nil payload header")
	}

	channelHdr, err := protoutil.UnmarshalChannelHeader(payload.Header.ChannelHeader)
	if err != nil {
		return nil, nil, errors.Wrap(err, "error unmarshaling channel header")
	}
	return payload, channelHdr, nil
}
