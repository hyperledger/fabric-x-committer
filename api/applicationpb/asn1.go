/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package applicationpb

import (
	"crypto/sha256"
	"encoding/asn1"

	"github.com/cockroachdb/errors"
)

// ASN1Digest digests a transactions for a given namespace index using ASN1 encoding and SHA256 digest.
func (ns *TxNamespace) ASN1Digest(txID string) ([]byte, error) {
	derBytes, err := ns.ASN1Marshal(txID)
	if err != nil {
		return nil, err
	}
	return digest(derBytes), nil
}

func digest(data []byte) []byte {
	h := sha256.New()
	h.Write(data) //nolint:revive,nolintlint // Hash write never fail.
	return h.Sum(nil)
}

// ASN1Marshal marshals a transactions for a given namespace index.
// It uses the schema described in asn1_tx_schema.asn.
func (ns *TxNamespace) ASN1Marshal(txID string) ([]byte, error) {
	ret, err := asn1.Marshal(*ns.translate(txID))
	return ret, errors.Wrap(err, "failed to marshal tx namespace")
}

// translate translates a TX namespace to a stab struct for asn1_tx_schema.asn.
// Any change to [Tx] requires a change to this method.
func (ns *TxNamespace) translate(txID string) *ASN1Namespace {
	n := ASN1Namespace{
		TxID:             txID,
		NamespaceID:      ns.NsId,
		NamespaceVersion: ProtoToAsnVersion(&ns.NsVersion),
		ReadsOnly:        make([]ASN1Read, len(ns.ReadsOnly)),
		ReadWrites:       make([]ASN1ReadWrite, len(ns.ReadWrites)),
		BlindWrites:      make([]ASN1Write, len(ns.BlindWrites)),
	}
	for i, r := range ns.ReadsOnly {
		n.ReadsOnly[i] = ASN1Read{
			Key:     r.Key,
			Version: ProtoToAsnVersion(r.Version),
		}
	}
	for i, rw := range ns.ReadWrites {
		n.ReadWrites[i] = ASN1ReadWrite{
			Key:     rw.Key,
			Version: ProtoToAsnVersion(rw.Version),
			Value:   rw.Value,
		}
	}
	for i, w := range ns.BlindWrites {
		n.BlindWrites[i] = ASN1Write{
			Key:   w.Key,
			Value: w.Value,
		}
	}
	return &n
}

// ProtoToAsnVersion converts the proto version to ASN.1 version.
// ASN.1 uses -1 to encode nil version.
func ProtoToAsnVersion(ver *uint64) int64 {
	if ver == nil {
		return -1
	}
	return int64(*ver) //nolint:gosec // ASN.1 does not support unsigned numbers.
}

// AsnToProtoVersion converts the ASN.1 version to proto version.
// ASN.1 uses -1 to encode nil version.
func AsnToProtoVersion(ver int64) *uint64 {
	if ver < 0 {
		return nil
	}
	protoVer := uint64(ver)
	return &protoVer
}

type (
	// ASN1Namespace is a stab for [Tx] and [TxNamespace].
	// Any change to these protobuf requires a change to these structures.
	// It conforms with asn1_tx_schema.asn.
	// We force the ASN.1 library to use UTF8 strings to avoid incompatibility with the schema.
	// If not specified, the library choose to use ASCII (PrintableString) for simple strings,
	// and UTF8 otherwise.
	ASN1Namespace struct {
		TxID             string `asn1:"utf8"`
		NamespaceID      string `asn1:"utf8"`
		NamespaceVersion int64
		ReadsOnly        []ASN1Read
		ReadWrites       []ASN1ReadWrite
		BlindWrites      []ASN1Write
	}
	// ASN1Read is a stab for [protoblocktx.Read].
	// Any change to this protobuf requires a change to these structures.
	ASN1Read struct {
		Key     []byte
		Version int64 `asn1:"optional,default:-1"`
	}
	// ASN1ReadWrite is a stab for [protoblocktx.ReadWrite].
	// Any change to this protobuf requires a change to these structures.
	ASN1ReadWrite struct {
		Key     []byte
		Value   []byte
		Version int64 `asn1:"optional,default:-1"`
	}
	// ASN1Write is a stab for [protoblocktx.Write].
	// Any change to this protobuf requires a change to these structures.
	ASN1Write struct {
		Key   []byte
		Value []byte
	}
)
