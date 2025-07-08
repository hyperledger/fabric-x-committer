/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package signature

import (
	"crypto/sha256"
	"encoding/asn1"

	"github.com/cockroachdb/errors"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
)

// DigestTxNamespace digests a transactions for a given namespace index.
func DigestTxNamespace(tx *protoblocktx.Tx, nsIndex int) ([]byte, error) {
	derBytes, err := ASN1MarshalTxNamespace(tx, nsIndex)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	h.Write(derBytes) //nolint:revive,nolintlint // Hash write never fail.
	return h.Sum(nil), nil
}

// ASN1MarshalTxNamespace marshals a transactions for a given namespace index.
// It uses the schema described in tx_schema.asn.
func ASN1MarshalTxNamespace(tx *protoblocktx.Tx, nsIndex int) ([]byte, error) {
	n, err := TranslateTx(tx, nsIndex)
	if err != nil {
		return nil, err
	}
	return asn1.Marshal(*n)
}

// TranslateTx translates a TX namespace to a stab struct for tx_schema.asn.
// Any change to [*protoblocktx.Tx] requires a change to this method.
func TranslateTx(tx *protoblocktx.Tx, nsIndex int) (*TxWithNamespace, error) {
	if nsIndex < 0 || nsIndex >= len(tx.Namespaces) {
		return nil, errors.New("namespace index out of range")
	}
	ns := tx.Namespaces[nsIndex]
	n := TxWithNamespace{
		TxID:             tx.Id,
		NamespaceID:      ns.NsId,
		NamespaceVersion: ProtoToAsnVersion(&ns.NsVersion),
		ReadsOnly:        make([]Read, len(ns.ReadsOnly)),
		ReadWrites:       make([]ReadWrite, len(ns.ReadWrites)),
		BlindWrites:      make([]Write, len(ns.BlindWrites)),
	}
	for i, r := range ns.ReadsOnly {
		n.ReadsOnly[i] = Read{
			Key:     r.Key,
			Version: ProtoToAsnVersion(r.Version),
		}
	}
	for i, rw := range ns.ReadWrites {
		n.ReadWrites[i] = ReadWrite{
			Key:     rw.Key,
			Version: ProtoToAsnVersion(rw.Version),
			Value:   rw.Value,
		}
	}
	for i, w := range ns.BlindWrites {
		n.BlindWrites[i] = Write{
			Key:   w.Key,
			Value: w.Value,
		}
	}
	return &n, nil
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
	// TxWithNamespace is a stab for [protoblocktx.Tx] and [protoblocktx.TxNamespace].
	// Any change to these protobuf requires a change to these structures.
	// It conforms with tx_schema.asn.
	// We force the ASN.1 library to use UTF8 strings to avoid incompatibility with the schema.
	// If not specified, the library choose to use ASCII (PrintableString) for simple strings,
	// and UTF8 otherwise.
	TxWithNamespace struct {
		TxID             string `asn1:"utf8"`
		NamespaceID      string `asn1:"utf8"`
		NamespaceVersion int64
		ReadsOnly        []Read
		ReadWrites       []ReadWrite
		BlindWrites      []Write
	}
	// Read is a stab for [protoblocktx.Read].
	// Any change to this protobuf requires a change to these structures.
	Read struct {
		Key     []byte
		Version int64 `asn1:"optional,default:-1"`
	}
	// ReadWrite is a stab for [protoblocktx.ReadWrite].
	// Any change to this protobuf requires a change to these structures.
	ReadWrite struct {
		Key     []byte
		Value   []byte
		Version int64 `asn1:"optional,default:-1"`
	}
	// Write is a stab for [protoblocktx.Write].
	// Any change to this protobuf requires a change to these structures.
	Write struct {
		Key   []byte
		Value []byte
	}
)
