/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package serialization

import (
	"github.com/cockroachdb/errors"
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
)

// UnmarshalTx unmarshal data bytes to protoblocktx.Tx.
func UnmarshalTx(data []byte) (*protoblocktx.Tx, error) {
	var tx protoblocktx.Tx
	if err := proto.Unmarshal(data, &tx); err != nil {
		return nil, errors.Wrap(err, "failed to unmarshal tx")
	}
	return &tx, nil
}
