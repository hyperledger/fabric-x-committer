/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package workload

import (
	"fmt"
	"strings"

	"github.com/cockroachdb/errors"
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
	"github.com/hyperledger/fabric-x-committer/api/types"
)

// CreateNamespacesTX creating the transaction containing the requested namespaces into the MetaNamespace.
func CreateNamespacesTX(policy *PolicyProfile) (*protoblocktx.Tx, error) {
	txSigner := NewTxSignerVerifier(policy)
	policyNamespaceSigner, ok := txSigner.HashSigners[types.MetaNamespaceID]
	if !ok {
		return nil, errors.New("no policy namespace signer found; cannot create namespaces")
	}

	readWrites := make([]*protoblocktx.ReadWrite, 0, len(txSigner.HashSigners))
	allNamespaces := make([]string, 0, len(txSigner.HashSigners))
	for ns, p := range txSigner.HashSigners {
		if ns == types.MetaNamespaceID {
			continue
		}
		policyBytes, err := proto.Marshal(p.GetVerificationPolicy())
		if err != nil {
			return nil, errors.New("failed to serialize namespace policy")
		}
		readWrites = append(readWrites, &protoblocktx.ReadWrite{
			Key:   []byte(ns),
			Value: policyBytes,
		})
		allNamespaces = append(allNamespaces, ns)
	}

	tx := &protoblocktx.Tx{
		Id: fmt.Sprintf("initial policy update: %v", strings.Join(allNamespaces, ",")),
		Namespaces: []*protoblocktx.TxNamespace{{
			NsId:       types.MetaNamespaceID,
			NsVersion:  0,
			ReadWrites: readWrites,
		}},
	}
	tx.Signatures = [][]byte{policyNamespaceSigner.Sign(tx, 0)}
	return tx, nil
}
