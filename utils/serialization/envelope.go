/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package serialization

import (
	"unicode/utf8"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/hyperledger/fabric-x-common/protoutil"
)

// EnvelopeLite holds the minimal fields extracted from a protobuf Envelope.
type EnvelopeLite struct {
	HeaderType int32
	TxID       string
	Data       []byte
}

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

// UnwrapEnvelopeLite extracts HeaderType, TxID, and payload Data from a
// serialized Envelope by walking the protobuf wire format directly, instead
// of performing 3 chained proto.Unmarshal calls (Envelope → Payload → ChannelHeader)
// like UnwrapEnvelope does. This reduces allocations and improves performance.
//
// The orderer already validates envelope structure (channelHeader, signatureHeader)
// before including transactions in a block, so the committer only receives valid
// envelopes. The only scenario where malformed envelopes could arrive is if BFT
// consensus itself is compromised (more than f malicious nodes), at which point
// the system has already failed. Therefore, this function only needs to handle
// well-formed input correctly and does not attempt to match proto.Unmarshal's
// error behavior on malformed input.
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
// returning immediately when the target field is found and skipping
// everything before it.
func UnwrapEnvelopeLite(message []byte) (*EnvelopeLite, error) {
	// Step 1: Read the Envelope. We need field 1 (payload).
	payloadBytes, err := extractBytesField(message, 1)
	if err != nil {
		return nil, errors.Wrap(err, "failed to parse envelope")
	}

	// Step 2: Read the Payload. We need field 1 (header) and field 2 (data).
	headerBytes, err := extractBytesField(payloadBytes, 1)
	if err != nil {
		return nil, errors.Wrap(err, "failed to parse payload")
	}
	dataBytes, err := extractBytesField(payloadBytes, 2)
	if err != nil {
		return nil, errors.Wrap(err, "failed to parse payload data")
	}

	// Step 3: Read the Header. We need field 1 (channel_header).
	channelHeaderBytes, err := extractBytesField(headerBytes, 1)
	if err != nil {
		return nil, errors.Wrap(err, "failed to parse header")
	}

	// Step 4: Read the ChannelHeader. We need field 1 (type) and field 5 (tx_id).
	headerTypeVal, err := extractVarintField(channelHeaderBytes, 1)
	if err != nil {
		return nil, errors.Wrap(err, "failed to parse channel header")
	}

	txID, err := extractStringField(channelHeaderBytes, 5)
	if err != nil {
		return nil, errors.Wrap(err, "failed to parse channel header tx_id")
	}

	return &EnvelopeLite{
		HeaderType: int32(headerTypeVal), //nolint:gosec // uint64 -> int32
		TxID:       txID,
		Data:       dataBytes,
	}, nil
}

// extractBytesField scans for targetField with bytes wire type and returns
// its decoded content. Returns nil if the field is absent.
// Returns an error only if the wire format is invalid before the target field.
func extractBytesField(b []byte, targetField protowire.Number) ([]byte, error) {
	for len(b) > 0 {
		// protowire.ConsumeXXX functions return a negative error code on failure.
		// On success, they return the number of bytes consumed (always >= 1),
		// so n is never zero.
		num, wtype, n := protowire.ConsumeTag(b)
		if n < 0 {
			return nil, errors.New("invalid protobuf tag")
		}
		b = b[n:]

		if num == targetField && wtype == protowire.BytesType {
			val, vn := protowire.ConsumeBytes(b)
			if vn < 0 {
				return nil, errors.New("invalid length-delimited field")
			}
			return val, nil
		}

		vn := protowire.ConsumeFieldValue(num, wtype, b)
		if vn < 0 {
			return nil, errors.New("invalid protobuf field value")
		}
		b = b[vn:]
	}
	return nil, nil
}

// extractVarintField scans for targetField with varint wire type and returns
// its value. Returns 0 if the field is absent (matching proto3 default).
// Returns an error only if the wire format is invalid before the target field.
func extractVarintField(b []byte, targetField protowire.Number) (uint64, error) {
	for len(b) > 0 {
		// See extractBytesField for why n is never zero.
		num, wtype, n := protowire.ConsumeTag(b)
		if n < 0 {
			return 0, errors.New("invalid protobuf tag")
		}
		b = b[n:]

		if num == targetField && wtype == protowire.VarintType {
			v, vn := protowire.ConsumeVarint(b)
			if vn < 0 {
				return 0, errors.New("invalid varint field")
			}
			return v, nil
		}

		vn := protowire.ConsumeFieldValue(num, wtype, b)
		if vn < 0 {
			return 0, errors.New("invalid protobuf field value")
		}
		b = b[vn:]
	}
	return 0, nil
}

// extractStringField scans for targetField with bytes wire type and returns
// its value as a string. Returns "" if the field is absent.
// Returns an error if the wire format is invalid or the value is not valid UTF-8.
func extractStringField(b []byte, targetField protowire.Number) (string, error) {
	val, err := extractBytesField(b, targetField)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(val) {
		return "", errors.New("string field contains invalid UTF-8")
	}
	return string(val), nil
}
