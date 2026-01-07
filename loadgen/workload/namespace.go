/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package workload

import (
	"slices"

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
	tx, err := CreateNamespacesTxFromEndorser(txb.TxEndorser, 0)
	if err != nil {
		return nil, err
	}
	return txb.MakeTx(tx), nil
}

// CreateNamespacesTX creating the transaction containing the requested namespaces into the MetaNamespace.
func CreateNamespacesTX(
	policy *PolicyProfile, metaNamespaceVersion uint64, includeNS ...string,
) (*applicationpb.Tx, error) {
	endorser := NewTxEndorser(policy)
	return CreateNamespacesTxFromEndorser(endorser, metaNamespaceVersion, includeNS...)
}

// CreateNamespacesTxFromEndorser creating the transaction containing the requested namespaces into the MetaNamespace.
func CreateNamespacesTxFromEndorser(
	endorser *TxEndorser, metaNamespaceVersion uint64, includeNS ...string,
) (*applicationpb.Tx, error) {
	readWrites := make([]*applicationpb.ReadWrite, 0, len(includeNS))
	for nsID, nsPolicy := range endorser.VerificationPolicies() {
		if (len(includeNS) > 0 && !slices.Contains(includeNS, nsID)) || nsID == committerpb.MetaNamespaceID {
			continue
		}
		policyBytes, err := proto.Marshal(nsPolicy)
		if err != nil {
			return nil, errors.Wrap(err, "failed to serialize namespace policy")
		}
		readWrites = append(readWrites, &applicationpb.ReadWrite{
			Key:   []byte(nsID),
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
