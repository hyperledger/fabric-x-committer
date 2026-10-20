/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package auth

import (
	"testing"
	"time"

	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/hyperledger/fabric-x-common/api/msppb"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/utils/statedb"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

// TestDatabaseDeleteExpired verifies expired records do not accumulate - a client that asks for a
// challenge and never authenticates must not leave a row behind forever - while live ones stay usable.
func TestDatabaseDeleteExpired(t *testing.T) {
	t.Parallel()
	db := newDatabaseForTest(t)
	ctx := t.Context()
	now := time.Now()

	live := testRecord(now.Add(time.Hour))
	liveToken, err := db.insertToken(ctx, live)
	require.NoError(t, err)
	expiredToken, err := db.insertToken(ctx, testRecord(now.Add(-time.Hour)))
	require.NoError(t, err)
	liveNonce, err := db.insertNonce(ctx, now.Add(time.Minute))
	require.NoError(t, err)
	expiredNonce, err := db.insertNonce(ctx, now.Add(-time.Minute))
	require.NoError(t, err)

	deletedTokens, deletedNonces, err := db.deleteExpired(ctx, now)
	require.NoError(t, err)
	require.Equal(t, int64(1), deletedTokens)
	require.Equal(t, int64(1), deletedNonces)

	_, err = db.readToken(ctx, expiredToken)
	require.ErrorIs(t, err, ErrTokenNotFound)
	got, err := db.readToken(ctx, liveToken)
	require.NoError(t, err)
	test.RequireProtoEqual(t, live, got)

	require.ErrorIs(t, db.consumeNonce(ctx, expiredNonce, now), errNonceNotFound)
	require.NoError(t, db.consumeNonce(ctx, liveNonce, now))
}

// TestDatabaseConsumeNonceRejects covers every nonce that must not redeem. "Already consumed" is the
// property the whole challenge rests on: a captured envelope carrying a spent nonce is worthless.
func TestDatabaseConsumeNonceRejects(t *testing.T) {
	t.Parallel()
	db := newDatabaseForTest(t)
	now := time.Now()

	expiredNonce, err := db.insertNonce(t.Context(), now.Add(-time.Minute))
	require.NoError(t, err)
	consumed, err := db.insertNonce(t.Context(), now.Add(time.Minute))
	require.NoError(t, err)
	require.NoError(t, db.consumeNonce(t.Context(), consumed, now))

	for _, tc := range []struct {
		name      string
		nonce     []byte
		consumeAt time.Time
	}{
		{name: "never issued", nonce: []byte("invented-nonce-value"), consumeAt: now},
		{name: "empty", nonce: nil, consumeAt: now},
		{name: "already consumed", nonce: consumed, consumeAt: now},
		{name: "expired", nonce: expiredNonce, consumeAt: now},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.ErrorIs(t, db.consumeNonce(t.Context(), tc.nonce, tc.consumeAt), errNonceNotFound)
		})
	}
}

// newDatabaseForTest returns the auth database over a freshly provisioned test database.
func newDatabaseForTest(t *testing.T) *database {
	t.Helper()
	dbConf := prepareTestDatabase(t)
	pool, err := statedb.NewPool(t.Context(), dbConf)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return &database{pool: pool, retry: dbConf.Retry}
}

// testRecord builds a token record with the given expiry.
func testRecord(expiresAt time.Time) *servicepb.TokenRecord {
	return &servicepb.TokenRecord{
		Identity:       &msppb.Identity{MspId: "Org1MSP"},
		CertHashSha256: []byte{0x01, 0x02, 0x03},
		Scope:          []string{committerpb.QueryService_GetRows_FullMethodName},
		ExpiresAt:      expiresAt.Unix(),
	}
}
