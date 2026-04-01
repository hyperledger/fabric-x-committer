/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package serialization

import (
	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"google.golang.org/protobuf/proto"
)

// UnmarshalTx unmarshal data bytes to protoblocktx.Tx.
func UnmarshalTx(data []byte) (*applicationpb.Tx, error) {
	var tx applicationpb.Tx
	if err := proto.Unmarshal(data, &tx); err != nil {
		return nil, errors.Wrap(err, "failed to unmarshal tx")
	}
	return &tx, nil
}
