/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package serialization

import (
	"errors"
	"fmt"

	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric/protoutil"
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
)

// NoOpSigner supports unsigned envelopes.
type NoOpSigner struct{}

// WrapEnvelopePayload serialize envelope.
func WrapEnvelopePayload(data []byte, header *common.Header) []byte {
	return protoutil.MarshalOrPanic(&common.Payload{
		Header: header,
		Data:   data,
	})
}

// WrapEnvelope wraps a payload with it's header and returns an envelope.
func WrapEnvelope(data []byte, header *common.Header) []byte {
	payloadBytes := WrapEnvelopePayload(data, header)

	envelope := &common.Envelope{
		Payload: payloadBytes,
	}
	return protoutil.MarshalOrPanic(envelope)
}

// UnwrapEnvelope deserialize an envelope.
func UnwrapEnvelope(message []byte) ([]byte, *common.ChannelHeader, error) {
	envelope, err := protoutil.GetEnvelopeFromBlock(message)
	if err != nil {
		return nil, nil, err
	}

	payload, channelHdr, err := ParseEnvelope(envelope)
	if err != nil {
		return nil, nil, err
	}

	return payload.Data, channelHdr, nil
}

// ParseEnvelope parse the envelope content.
func ParseEnvelope(envelope *common.Envelope) (*common.Payload, *common.ChannelHeader, error) {
	payload, err := protoutil.UnmarshalPayload(envelope.Payload)
	if err != nil {
		return nil, nil, err
	}

	if payload.Header == nil {
		return nil, nil, errors.New("payload header is nil")
	}

	channelHdr, err := protoutil.UnmarshalChannelHeader(payload.Header.ChannelHeader)
	if err != nil {
		return nil, nil, err
	}
	return payload, channelHdr, nil
}

// CreateEnvelope create an envelope with or without a signature and the corresponding header.
// An unsigned envelope can only be used with a patched fabric orderer.
// It's output envelope is different from [protoutil.CreateSignedEnvelope]:
//   - This method adds TxId to the channel's header
//   - This method's payload is the given message and not [common.Payload]
func CreateEnvelope(
	channelID string,
	signer protoutil.Signer,
	protoMsg proto.Message,
) (*common.Envelope, string, error) {
	if signer == nil {
		signer = &NoOpSigner{}
	}
	data, err := proto.Marshal(protoMsg)
	if err != nil {
		return nil, "", fmt.Errorf("error marshaling: %w", err)
	}
	signatureHeader := protoutil.NewSignatureHeaderOrPanic(signer)
	channelHeader := protoutil.MakeChannelHeader(common.HeaderType_MESSAGE, 0, channelID, 0)
	switch msg := protoMsg.(type) {
	case *protoblocktx.Tx:
		channelHeader.TxId = msg.Id
	default:
		channelHeader.TxId = protoutil.ComputeTxID(signatureHeader.Nonce, signatureHeader.Creator)
	}
	payloadHeader := protoutil.MakePayloadHeader(channelHeader, signatureHeader)
	payload := WrapEnvelopePayload(data, payloadHeader)

	signature, err := signer.Sign(payload)
	if err != nil {
		return nil, channelHeader.TxId, fmt.Errorf("error signing payload: %w", err)
	}
	return &common.Envelope{Payload: payload, Signature: signature}, channelHeader.TxId, nil
}

// Serialize is an empty implementations of the singer API.
func (*NoOpSigner) Serialize() ([]byte, error) { return []byte{}, nil }

// Sign is an empty implementations of the singer API.
func (*NoOpSigner) Sign([]byte) ([]byte, error) { return nil, nil }
