/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package workload

import (
	"github.com/cockroachdb/errors"
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-committer/api/applicationpb"
	"github.com/hyperledger/fabric-x-committer/api/committerpb"
	"github.com/hyperledger/fabric-x-committer/api/servicepb"
)

// CreateLoadGenNamespacesTX creating the transaction containing the requested namespaces into the MetaNamespace.
func CreateLoadGenNamespacesTX(policy *PolicyProfile) (*servicepb.LoadGenTx, error) {
	txb, txbErr := NewTxBuilderFromPolicy(policy, nil)
	if txbErr != nil {
		return nil, txbErr
	}
	tx, err := CreateNamespacesTxFromSigner(txb.TxSigner, 0)
	if err != nil {
		return nil, err
	}
	return txb.MakeTx(tx), nil
}

// CreateNamespacesTX creating the transaction containing the requested namespaces into the MetaNamespace.
func CreateNamespacesTX(
	policy *PolicyProfile, metaNamespaceVersion uint64, includeNS ...string,
) (*applicationpb.Tx, error) {
	signer := NewTxSignerVerifier(policy)
	return CreateNamespacesTxFromSigner(signer, metaNamespaceVersion, includeNS...)
}

// CreateNamespacesTxFromSigner creating the transaction containing the requested namespaces into the MetaNamespace.
func CreateNamespacesTxFromSigner(
	signer *TxSignerVerifier, metaNamespaceVersion uint64, includeNS ...string,
) (*applicationpb.Tx, error) {
	if len(includeNS) == 0 {
		includeNS = signer.AllNamespaces()
	}

	readWrites := make([]*applicationpb.ReadWrite, 0, len(includeNS))
	for _, ns := range includeNS {
		if ns == committerpb.MetaNamespaceID {
			continue
		}
		policyBytes, err := proto.Marshal(signer.Policy(ns).VerificationPolicy())
		if err != nil {
			return nil, errors.Wrap(err, "failed to serialize namespace policy")
		}
		readWrites = append(readWrites, &applicationpb.ReadWrite{
			Key:   []byte(ns),
			Value: policyBytes,
		})
	}

	return &applicationpb.Tx{
		Namespaces: []*applicationpb.TxNamespace{{
			NsId:       committerpb.MetaNamespaceID,
			NsVersion:  metaNamespaceVersion,
			ReadWrites: readWrites,
		}},
	}, nil
}
