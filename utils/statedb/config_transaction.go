/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package statedb

import (
	"context"
	"fmt"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/yugabyte/pgx/v5/pgxpool"
)

// selectConfigTransaction reads the committed configuration transaction by its well-known key.
var selectConfigTransaction = fmt.Sprintf(
	"SELECT value, version FROM %s WHERE key = $1", TableName(committerpb.ConfigNamespaceID),
)

// ReadConfigTransaction reads the latest committed channel-configuration transaction, returning a nil
// envelope - not an error - during bootstrap. Shared, so no two readers disagree on the authoritative row.
func ReadConfigTransaction(ctx context.Context, pool *pgxpool.Pool) (*applicationpb.ConfigTransaction, error) {
	rows, err := pool.Query(ctx, selectConfigTransaction, []byte(committerpb.ConfigKey))
	if err != nil {
		return nil, errors.Wrap(err, "failed to query the config transaction")
	}
	defer rows.Close()

	configTX := &applicationpb.ConfigTransaction{}
	if rows.Next() {
		if err = rows.Scan(&configTX.Envelope, &configTX.Version); err != nil {
			return nil, errors.Wrap(err, "failed to scan the config transaction")
		}
	}
	return configTX, errors.Wrap(rows.Err(), "failed while reading the config transaction")
}
