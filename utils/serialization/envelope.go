/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package serialization

import (
	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"google.golang.org/protobuf/encoding/protowire"

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

// EnvelopeLite holds the minimal fields extracted from a protobuf Envelope
// without performing full deserialization.
type EnvelopeLite struct {
	HeaderType int32
	TxID       string
	Data       []byte
}

// UnwrapEnvelopeLite extracts HeaderType, TxID, and payload Data from a
// serialized Envelope by walking the protobuf wire format directly, instead
// of performing 3 chained proto.Unmarshal calls (Envelope → Payload → ChannelHeader)
// like UnwrapEnvelope does. This reduces allocations and improves the performance
// by 4x.
//
// The function navigates through the following nested proto messages,
// extracting only the fields it needs (marked with ←) and skipping the rest:
//
//	message Envelope {
//	    bytes payload = 1;   // serialized Payload
//	    bytes signature = 2;
//	}
//
//	message Payload {
//	    Header header = 1;   // serialized Header
//	    bytes data = 2;      // ← EnvelopeLite.Data
//	}
//
//	message Header {
//	    bytes channel_header = 1;   // serialized ChannelHeader
//	    bytes signature_header = 2;
//	}
//
//	message ChannelHeader {
//	    int32 type = 1;                          // ← EnvelopeLite.HeaderType
//	    int32 version = 2;
//	    google.protobuf.Timestamp timestamp = 3;
//	    string channel_id = 4;
//	    string tx_id = 5;                        // ← EnvelopeLite.TxID
//	    uint64 epoch = 6;
//	    bytes extension = 7;
//	    bytes tls_cert_hash = 8;
//	}
//
// Each protobuf field is encoded on the wire as:
//
//	[tag] [value]
//
// The tag is a varint encoding (field_number << 3 | wire_type).
//
// For bytes/string fields, the value is a length prefix followed by the raw bytes:
//
//	[tag: 1 byte] [length: varint] [data: length bytes]
//
// For varint fields (like int32), the value is just the varint itself:
//
//	[tag: 1 byte] [value: varint]
//
// For example, an Envelope with Type=3 (ENDORSER_TRANSACTION), TxID="tx1",
// and Data="hello" encodes as:
//
//	Envelope (20 bytes):
//	  0a    = tag: field_number=1, wire_type=2 (bytes)   [1<<3|2 = 0x0a]
//	  12    = length: 18 bytes
//	  [...] = Payload (serialized, 18 bytes)             → Envelope.payload
//
//	  Payload (18 bytes, nested inside Envelope):
//	    0a    = tag: field_number=1, wire_type=2 (bytes) [1<<3|2 = 0x0a]
//	    09    = length: 9 bytes
//	    [...] = Header (serialized, 9 bytes)             → Payload.header
//
//	    12    = tag: field_number=2, wire_type=2 (bytes) [2<<3|2 = 0x12]
//	    05    = length: 5 bytes
//	    68656c6c6f = "hello"                             → Payload.data ←
//
//	    Header (9 bytes, nested inside Payload):
//	      0a    = tag: field_number=1, wire_type=2 (bytes) [1<<3|2 = 0x0a]
//	      07    = length: 7 bytes
//	      [...] = ChannelHeader (serialized, 7 bytes)      → Header.channel_header
//
//	      ChannelHeader (7 bytes, nested inside Header):
//	        08  = tag: field_number=1, wire_type=0 (varint) [1<<3|0 = 0x08]
//	        03  = value: 3 (ENDORSER_TRANSACTION)           → ChannelHeader.type ←
//
//	        2a  = tag: field_number=5, wire_type=2 (bytes)  [5<<3|2 = 0x2a]
//	        03  = length: 3 bytes
//	        747831 = "tx1"                                  → ChannelHeader.tx_id ←
//
// The function reads tags sequentially at each nesting level using protowire,
// matches the field numbers from the proto definitions above, and skips
// everything else.
func UnwrapEnvelopeLite(message []byte) (*EnvelopeLite, error) {
	// Step 1: Read the Envelope. The input bytes are a serialized Envelope message.
	// We need field 1 (payload) -- extractBytesField scans for field 1 with bytes wire type and returns its value.
	payloadBytes, err := extractBytesField(message, 1)
	if err != nil || payloadBytes == nil {
		return nil, errors.New("failed to extract payload from envelope")
	}

	// Step 2: Read the Payload. We need two fields from it:
	//   field 1 (header) and field 2 (data).
	// Note: This scans payloadBytes twice (once per field). A single-pass loop
	// that extracts both fields simultaneously is ~5 ns faster, but the payload
	// is small enough that the difference is negligible. We chose to reuse
	// extractBytesField for simplicity.
	headerBytes, err := extractBytesField(payloadBytes, 1)
	if err != nil || headerBytes == nil {
		return nil, errors.New("missing header in payload")
	}
	// Data may be absent (nil) if the Payload has no data — that's valid.
	dataBytes, err := extractBytesField(payloadBytes, 2)
	if err != nil {
		return nil, errors.New("invalid data in payload")
	}

	// Step 3: Read the Header. We need field 1 (channel_header).
	channelHeaderBytes, err := extractBytesField(headerBytes, 1)
	if err != nil {
		return nil, errors.New("failed to extract channel header")
	}
	// When all ChannelHeader fields are zero/default, proto3 omits the field entirely,
	// so channelHeaderBytes is nil. This is valid and means Type=0 and TxID="".
	if channelHeaderBytes == nil {
		return &EnvelopeLite{Data: dataBytes}, nil
	}

	// Step 4: Read the ChannelHeader. We need field 1 (type) and field 5 (tx_id).
	// After extracting field 1, we advance past its value so the subsequent
	// search for field 5 doesn't revisit it. This is safe because protobuf
	// serializers write fields in field-number order.
	var headerType int32
	remaining := channelHeaderBytes
	raw, err := extractField(channelHeaderBytes, 1, protowire.VarintType)
	if err != nil {
		return nil, errors.Wrap(err, "failed to parse channel header type")
	}
	if raw != nil {
		v, n := protowire.ConsumeVarint(raw)
		if n < 0 {
			return nil, errors.New("invalid varint for type field")
		}
		headerType = int32(v) //nolint:gosec // uint64 -> int32
		remaining = raw[n:]
	}

	txIDBytes, err := extractBytesField(remaining, 5)
	if err != nil {
		return nil, errors.Wrap(err, "failed to parse channel header tx_id")
	}

	return &EnvelopeLite{
		HeaderType: headerType,
		TxID:       string(txIDBytes),
		Data:       dataBytes,
	}, nil
}

// extractBytesField scans for targetField with bytes wire type and returns
// its decoded content. Returns (nil, nil) if the field is not found.
func extractBytesField(b []byte, targetField protowire.Number) ([]byte, error) {
	raw, err := extractField(b, targetField, protowire.BytesType)
	if err != nil || raw == nil {
		return nil, err
	}
	val, n := protowire.ConsumeBytes(raw)
	if n < 0 {
		return nil, errors.New("invalid protobuf bytes field")
	}
	return val, nil
}

// extractField scans a protobuf wire-format message for the first occurrence
// of targetField with the given expectedType. It returns the unconsumed slice
// starting at the field's value, so the caller can decode it with the
// appropriate protowire.ConsumeXxx function.
func extractField(b []byte, targetField protowire.Number, expectedType protowire.Type) ([]byte, error) {
	for len(b) > 0 {
		num, wtype, n := protowire.ConsumeTag(b)
		if n < 0 {
			return nil, errors.New("invalid protobuf tag")
		}
		b = b[n:]

		if num == targetField {
			if wtype != expectedType {
				return nil, errors.Newf("field %d: expected wire type %d, got %d", targetField, expectedType, wtype)
			}
			return b, nil
		}

		vn := protowire.ConsumeFieldValue(num, wtype, b)
		if vn < 0 {
			return nil, errors.New("invalid protobuf field value")
		}
		b = b[vn:]
	}
	return nil, nil
}
