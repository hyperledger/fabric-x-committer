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
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-common/protoutil"

	"github.com/hyperledger/fabric-x-committer/api/serializationpb"
)

// EnvelopeLite holds the minimal fields extracted from a protobuf Envelope.
type EnvelopeLite struct {
	HeaderType int32
	TxID       string
	Data       []byte
}

// UnwrapEnvelope extracts HeaderType, TxID, and payload Data from a serialized
// Envelope using proto.Unmarshal with projection protos that declare only the
// fields the committer needs. Unknown fields (channel_id, timestamp, epoch, etc.)
// are silently skipped by proto.Unmarshal, making this function's acceptance
// semantics identical to UnwrapEnvelopeLite's wire scanning.
func UnwrapEnvelope(message []byte) (*EnvelopeLite, error) {
	var env serializationpb.EnvelopeLiteProto
	if err := proto.Unmarshal(message, &env); err != nil {
		return nil, errors.Wrap(err, "failed to unmarshal envelope")
	}

	var payload serializationpb.PayloadLite
	if err := proto.Unmarshal(env.Payload, &payload); err != nil {
		return nil, errors.Wrap(err, "failed to unmarshal payload")
	}

	var header serializationpb.HeaderLite
	if err := proto.Unmarshal(payload.Header, &header); err != nil {
		return nil, errors.Wrap(err, "failed to unmarshal header")
	}

	var chdr serializationpb.ChannelHeaderLite
	if err := proto.Unmarshal(header.ChannelHeader, &chdr); err != nil {
		return nil, errors.Wrap(err, "failed to unmarshal channel header")
	}

	return &EnvelopeLite{
		HeaderType: chdr.Type,
		TxID:       chdr.TxId,
		Data:       payload.Data,
	}, nil
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
	var headerType int32
	raw, err := extractField(channelHeaderBytes, 1, protowire.VarintType)
	if err != nil {
		return nil, errors.Wrap(err, "failed to parse channel header")
	}
	if raw != nil {
		v, n := protowire.ConsumeVarint(raw)
		if n <= 0 {
			return nil, errors.New("invalid varint for type field")
		}
		headerType = int32(v) //nolint:gosec // uint64 -> int32
	}

	txID, err := extractStringField(channelHeaderBytes, 5)
	if err != nil {
		return nil, errors.Wrap(err, "failed to parse channel header tx_id")
	}

	return &EnvelopeLite{
		HeaderType: headerType,
		TxID:       txID,
		Data:       dataBytes,
	}, nil
}

// maxValidFieldNumber is the largest field number allowed by the protobuf spec.
// proto.Unmarshal rejects fields outside the range [1, 2^29-1].
const maxValidFieldNumber protowire.Number = 1<<29 - 1

// extractBytesField scans for targetField with bytes wire type and returns
// its decoded content. Returns nil if the field is absent or zero-length
// (matching proto3 semantics where both represent the default value).
// Returns an error only if the wire format is invalid.
func extractBytesField(b []byte, targetField protowire.Number) ([]byte, error) {
	raw, err := extractField(b, targetField, protowire.BytesType)
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, nil
	}
	val, n := protowire.ConsumeBytes(raw)
	if n < 0 {
		return nil, errors.New("invalid length-delimited field")
	}
	return val, nil
}

// extractField scans for the last occurrence of targetField with expectedType,
// returning the unconsumed slice starting at the field's value. Returns nil
// if not found. Returns an error only if the wire format is invalid.
func extractField(b []byte, targetField protowire.Number, expectedType protowire.Type) ([]byte, error) {
	var lastMatch []byte
	err := scanField(b, targetField, expectedType, func(b []byte) error {
		lastMatch = b
		return nil
	})
	return lastMatch, err
}

// extractStringField scans for targetField and returns its value as a string,
// validating UTF-8 for ALL occurrences of the field (not just the last one).
// This matches proto.Unmarshal's behavior of rejecting the entire message if
// any occurrence of a string field contains invalid UTF-8.
func extractStringField(b []byte, targetField protowire.Number) (string, error) {
	var lastMatch string
	err := scanField(b, targetField, protowire.BytesType, func(b []byte) error {
		val, n := protowire.ConsumeBytes(b)
		if n < 0 {
			return errors.New("invalid length-delimited field")
		}
		if !utf8.Valid(val) {
			return errors.New("string field contains invalid UTF-8")
		}
		lastMatch = string(val)
		return nil
	})
	return lastMatch, err
}

// scanField iterates a protobuf wire-format message and calls onMatch(b) for
// each occurrence of targetField with the given targetType, where b is the
// unconsumed slice starting at the field's value. It validates the entire
// wire structure, rejecting malformed data even in non-target fields.
//
// The full scan (rather than returning on first match) is intentional:
// the protobuf spec does not guarantee field ordering — while proto.Marshal
// writes fields in ascending order, proto.Unmarshal accepts any order and
// uses last-writer-wins for duplicate scalar fields. We match that behavior.
func scanField(b []byte, targetField protowire.Number, targetType protowire.Type, onMatch func(b []byte) error) error {
	for len(b) > 0 {
		num, wtype, n := protowire.ConsumeTag(b)
		if n < 0 {
			return errors.New("invalid protobuf tag")
		}
		b = b[n:]

		if num < 1 || num > maxValidFieldNumber {
			return errors.New("protobuf field number out of range")
		}

		if num == targetField && wtype == targetType {
			if err := onMatch(b); err != nil {
				return err
			}
		}

		// Skip fields we don't need, including fields with the right number but
		// wrong wire type (matching proto3's lenient decoding behavior).
		vn := protowire.ConsumeFieldValue(num, wtype, b)
		if vn < 0 {
			return errors.New("invalid protobuf field value")
		}
		b = b[vn:]
	}
	return nil
}
