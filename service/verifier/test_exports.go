/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package verifier

import (
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
	"github.com/hyperledger/fabric-x-committer/api/types"
)

var ns1 = "1"

// BadTxFormatTestCases holds the test cases needed to test bad transaction formats.
var BadTxFormatTestCases = []struct {
	Tx             *protoblocktx.Tx
	ExpectedStatus protoblocktx.Status
}{
	{
		Tx: &protoblocktx.Tx{
			Id: "",
		},
		ExpectedStatus: protoblocktx.Status_ABORTED_MISSING_TXID,
	},
	{
		Tx: &protoblocktx.Tx{
			Id: "empty namespaces",
		},
		ExpectedStatus: protoblocktx.Status_ABORTED_EMPTY_NAMESPACES,
	},
	{
		Tx: &protoblocktx.Tx{
			Id: "invalid signature",
			Namespaces: []*protoblocktx.TxNamespace{
				{
					NsId:       ns1,
					NsVersion:  0,
					ReadWrites: []*protoblocktx.ReadWrite{{Key: []byte("1")}},
				},
			},
			Signatures: [][]byte{[]byte("dummy")},
		},
		ExpectedStatus: protoblocktx.Status_ABORTED_SIGNATURE_INVALID,
	},
	{
		Tx: &protoblocktx.Tx{
			Id: "namespace id is invalid in tx",
			Namespaces: []*protoblocktx.TxNamespace{
				{
					NsId: "//",
				},
			},
			Signatures: [][]byte{
				[]byte("dummy"),
			},
		},
		ExpectedStatus: protoblocktx.Status_ABORTED_NAMESPACE_ID_INVALID,
	},
	{
		Tx: &protoblocktx.Tx{
			Id: "no writes",
			Namespaces: []*protoblocktx.TxNamespace{
				{
					NsId:      ns1,
					NsVersion: 0,
					ReadsOnly: []*protoblocktx.Read{
						{
							Key: []byte("k1"),
						},
					},
				},
			},
			Signatures: [][]byte{
				[]byte("dummy"),
			},
		},
		ExpectedStatus: protoblocktx.Status_ABORTED_NO_WRITES,
	},
	{
		Tx: &protoblocktx.Tx{
			Id: "namespace id is invalid in metaNs tx",
			Namespaces: []*protoblocktx.TxNamespace{
				{
					NsId:      ns1,
					NsVersion: 0,
					ReadWrites: []*protoblocktx.ReadWrite{
						{
							Key: []byte("key"),
						},
					},
				},
				{
					NsId:      types.MetaNamespaceID,
					NsVersion: 0,
					ReadWrites: []*protoblocktx.ReadWrite{
						{
							Key: []byte("/\\"),
						},
					},
				},
			},
			Signatures: [][]byte{
				[]byte("dummy"),
				[]byte("dummy"),
			},
		},
		ExpectedStatus: protoblocktx.Status_ABORTED_NAMESPACE_ID_INVALID,
	},
	{
		Tx: &protoblocktx.Tx{
			Id: "namespace policy is invalid in metaNs tx",
			Namespaces: []*protoblocktx.TxNamespace{
				{
					NsId:      ns1,
					NsVersion: 0,
					ReadWrites: []*protoblocktx.ReadWrite{
						{
							Key: []byte("key"),
						},
					},
				},
				{
					NsId:      types.MetaNamespaceID,
					NsVersion: 0,
					ReadWrites: []*protoblocktx.ReadWrite{
						{
							Key:   []byte("2"),
							Value: []byte("value"),
						},
					},
				},
			},
			Signatures: [][]byte{
				[]byte("dummy"),
				[]byte("dummy"),
			},
		},
		ExpectedStatus: protoblocktx.Status_ABORTED_NAMESPACE_POLICY_INVALID,
	},
	{
		Tx: &protoblocktx.Tx{
			Id: "duplicate namespace",
			Namespaces: []*protoblocktx.TxNamespace{
				{
					NsId:      ns1,
					NsVersion: 0,
					ReadWrites: []*protoblocktx.ReadWrite{
						{
							Key: []byte("key"),
						},
					},
				},
				{
					NsId:      types.MetaNamespaceID,
					NsVersion: 0,
					ReadWrites: []*protoblocktx.ReadWrite{
						{
							Key:   []byte("2"),
							Value: defaultNsPolicy(),
						},
					},
				},
				{
					NsId:      ns1,
					NsVersion: 0,
				},
			},
			Signatures: [][]byte{
				[]byte("dummy"),
				[]byte("dummy"),
				[]byte("dummy"),
			},
		},
		ExpectedStatus: protoblocktx.Status_ABORTED_DUPLICATE_NAMESPACE,
	},
	{
		Tx: &protoblocktx.Tx{
			Id: "blind writes not allowed in metaNs tx",
			Namespaces: []*protoblocktx.TxNamespace{
				{
					NsId:      ns1,
					NsVersion: 0,
					ReadWrites: []*protoblocktx.ReadWrite{
						{
							Key: []byte("key"),
						},
					},
				},
				{
					NsId:      types.MetaNamespaceID,
					NsVersion: 0,
					BlindWrites: []*protoblocktx.Write{
						{
							Key:   []byte("2"),
							Value: defaultNsPolicy(),
						},
					},
				},
			},
			Signatures: [][]byte{
				[]byte("dummy"),
				[]byte("dummy"),
			},
		},
		ExpectedStatus: protoblocktx.Status_ABORTED_BLIND_WRITES_NOT_ALLOWED,
	},
	{
		Tx: &protoblocktx.Tx{
			Id: "nil key in ReadOnly",
			Namespaces: []*protoblocktx.TxNamespace{
				{
					NsId:       ns1,
					NsVersion:  0,
					ReadsOnly:  []*protoblocktx.Read{{Key: nil}},
					ReadWrites: []*protoblocktx.ReadWrite{{Key: []byte("1")}},
				},
			},
			Signatures: [][]byte{[]byte("dummy")},
		},
		ExpectedStatus: protoblocktx.Status_ABORTED_NIL_KEY,
	},
	{
		Tx: &protoblocktx.Tx{
			Id: "nil key in ReadWrites",
			Namespaces: []*protoblocktx.TxNamespace{
				{
					NsId:       ns1,
					NsVersion:  0,
					ReadWrites: []*protoblocktx.ReadWrite{{Key: nil}},
				},
			},
			Signatures: [][]byte{[]byte("dummy")},
		},
		ExpectedStatus: protoblocktx.Status_ABORTED_NIL_KEY,
	},
	{
		Tx: &protoblocktx.Tx{
			Id: "nil key in BlindWrites",
			Namespaces: []*protoblocktx.TxNamespace{
				{
					NsId:        ns1,
					NsVersion:   0,
					BlindWrites: []*protoblocktx.Write{{Key: nil}},
				},
			},
			Signatures: [][]byte{[]byte("dummy")},
		},
		ExpectedStatus: protoblocktx.Status_ABORTED_NIL_KEY,
	},
	{
		Tx: &protoblocktx.Tx{
			Id: "duplicate key within ReadOnly",
			Namespaces: []*protoblocktx.TxNamespace{
				{
					NsId:       ns1,
					NsVersion:  0,
					ReadsOnly:  []*protoblocktx.Read{{Key: []byte("key1")}, {Key: []byte("key1")}},
					ReadWrites: []*protoblocktx.ReadWrite{{Key: []byte("1")}},
				},
			},
			Signatures: [][]byte{[]byte("dummy")},
		},
		ExpectedStatus: protoblocktx.Status_ABORTED_DUPLICATE_KEY_IN_READ_WRITE_SET,
	},
	{
		Tx: &protoblocktx.Tx{
			Id: "duplicate key within ReadWrite",
			Namespaces: []*protoblocktx.TxNamespace{
				{
					NsId:       ns1,
					NsVersion:  0,
					ReadWrites: []*protoblocktx.ReadWrite{{Key: []byte("key1")}, {Key: []byte("key1")}},
				},
			},
			Signatures: [][]byte{[]byte("dummy")},
		},
		ExpectedStatus: protoblocktx.Status_ABORTED_DUPLICATE_KEY_IN_READ_WRITE_SET,
	},
	{
		Tx: &protoblocktx.Tx{
			Id: "duplicate key within BlindWrites",
			Namespaces: []*protoblocktx.TxNamespace{
				{
					NsId:        ns1,
					NsVersion:   0,
					BlindWrites: []*protoblocktx.Write{{Key: []byte("key1")}, {Key: []byte("key1")}},
				},
			},
			Signatures: [][]byte{[]byte("dummy")},
		},
		ExpectedStatus: protoblocktx.Status_ABORTED_DUPLICATE_KEY_IN_READ_WRITE_SET,
	},
	{
		Tx: &protoblocktx.Tx{
			Id: "duplicate key across ReadOnly and ReadWrite",
			Namespaces: []*protoblocktx.TxNamespace{
				{
					NsId:       ns1,
					NsVersion:  0,
					ReadsOnly:  []*protoblocktx.Read{{Key: []byte("key1")}},
					ReadWrites: []*protoblocktx.ReadWrite{{Key: []byte("key1")}},
				},
			},
			Signatures: [][]byte{[]byte("dummy")},
		},
		ExpectedStatus: protoblocktx.Status_ABORTED_DUPLICATE_KEY_IN_READ_WRITE_SET,
	},
	{
		Tx: &protoblocktx.Tx{
			Id: "duplicate key across ReadWrite and BlindWrites",
			Namespaces: []*protoblocktx.TxNamespace{
				{
					NsId:        ns1,
					NsVersion:   0,
					ReadWrites:  []*protoblocktx.ReadWrite{{Key: []byte("key1")}},
					BlindWrites: []*protoblocktx.Write{{Key: []byte("key1")}},
				},
			},
			Signatures: [][]byte{[]byte("dummy")},
		},
		ExpectedStatus: protoblocktx.Status_ABORTED_DUPLICATE_KEY_IN_READ_WRITE_SET,
	},
}

func defaultNsPolicy() []byte {
	nsPolicy, _ := proto.Marshal(&protoblocktx.NamespacePolicy{
		Scheme:    "ECDSA",
		PublicKey: []byte("publicKey"),
	})
	return nsPolicy
}
