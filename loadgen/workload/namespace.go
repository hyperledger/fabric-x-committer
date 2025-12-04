/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package workload

import (
	"maps"
	"slices"

	"github.com/cockroachdb/errors"
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-committer/api/applicationpb"
	"github.com/hyperledger/fabric-x-committer/api/protoloadgen"
	"github.com/hyperledger/fabric-x-committer/api/types"
)

// CreateLoadGenNamespacesTX creating the transaction containing the requested namespaces into the MetaNamespace.
func CreateLoadGenNamespacesTX(policy *PolicyProfile) (*protoloadgen.TX, error) {
	txb, txbErr := NewTxBuilderFromPolicy(policy, nil)
	if txbErr != nil {
		return nil, txbErr
	}
	_, ok := txb.TxSigner.HashSigners[types.MetaNamespaceID]
	if !ok {
		return nil, errors.New("no meta namespace signer found; cannot create namespaces")
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
		includeNS = slices.Collect(maps.Keys(signer.HashSigners))
	}

	readWrites := make([]*applicationpb.ReadWrite, 0, len(signer.HashSigners))
	for _, ns := range includeNS {
		if ns == types.MetaNamespaceID {
			continue
		}
		p, ok := signer.HashSigners[ns]
		if !ok {
			return nil, errors.Errorf("no policy for '%s'", ns)
		}
		policyBytes, err := proto.Marshal(p.GetVerificationPolicy())
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
			NsId:       types.MetaNamespaceID,
			NsVersion:  metaNamespaceVersion,
			ReadWrites: readWrites,
		}},
	}, nil
}
