/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/jackc/puddle/v2"
	"github.com/yugabyte/pgx/v5"
	"github.com/yugabyte/pgx/v5/pgconn"
	"github.com/yugabyte/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/utils/retry"
)

const (
	tokensTable = "auth_tokens" //nolint:gosec // The table names are not sensitive information.
	noncesTable = "auth_nonces"

	sqlInsertToken         = "INSERT INTO " + tokensTable + " (token_hash, record, expires_at) VALUES ($1, $2, $3)"
	sqlSelectToken         = "SELECT record FROM " + tokensTable + " WHERE token_hash = $1"
	sqlDeleteExpiredTokens = "DELETE FROM " + tokensTable + " WHERE expires_at < $1"

	sqlInsertNonce         = "INSERT INTO " + noncesTable + " (nonce, expires_at) VALUES ($1, $2)"
	sqlConsumeNonce        = "DELETE FROM " + noncesTable + " WHERE nonce = $1 AND expires_at >= $2"
	sqlDeleteExpiredNonces = "DELETE FROM " + noncesTable + " WHERE expires_at < $1"

	// randomLength is the size of an issued token or nonce.
	randomLength = 32
)

var (
	// ErrTokenNotFound is returned when a token record is absent from the store, meaning the token was
	// never issued by this deployment or has expired and been swept.
	ErrTokenNotFound = errors.New("token record not found")

	// errNonceNotFound is returned when a nonce is unknown, already consumed, or expired. The three are
	// deliberately indistinguishable to the caller, so a client cannot probe which nonces exist.
	errNonceNotFound = errors.New("authentication nonce is unknown, already used, or expired")
)

// database persists tokens and nonces in the shared state database, so any instance can serve any client.
type database struct {
	pool  *pgxpool.Pool
	retry *retry.Profile
}

// insertToken stores rec under a newly generated token and returns the token. Each attempt draws a fresh
// token and fails on a conflict, so a colliding token never resolves to another client's record.
func (db *database) insertToken(ctx context.Context, rec *servicepb.TokenRecord) (string, error) {
	data, err := proto.Marshal(rec)
	if err != nil {
		return "", errors.Wrap(err, "failed to marshal token record")
	}
	return retry.ExecuteWithResult(ctx, db.retry, func() (string, error) {
		b, randErr := randomBytes()
		if randErr != nil {
			return "", randErr
		}
		token := base64.RawURLEncoding.EncodeToString(b)
		if _, execErr := db.pool.Exec(ctx, sqlInsertToken, hashToken(token), data, rec.GetExpiresAt()); execErr != nil {
			return "", errors.Wrap(execErr, "failed to persist token record")
		}
		return token, nil
	}, puddle.ErrClosedPool)
}

// readToken returns the record for a presented token, or ErrTokenNotFound when it is absent.
func (db *database) readToken(ctx context.Context, token string) (*servicepb.TokenRecord, error) {
	records, err := retry.ExecuteWithResult(ctx, db.retry, func() ([][]byte, error) {
		rows, queryErr := db.pool.Query(ctx, sqlSelectToken, hashToken(token))
		if queryErr != nil {
			return nil, errors.Wrap(queryErr, "failed to read token record")
		}
		collected, collectErr := pgx.CollectRows(rows, pgx.RowTo[[]byte])
		return collected, errors.Wrap(collectErr, "failed to read token record")
	}, puddle.ErrClosedPool)
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, ErrTokenNotFound
	}

	rec := &servicepb.TokenRecord{}
	if err = proto.Unmarshal(records[0], rec); err != nil {
		return nil, errors.Wrap(err, "failed to unmarshal token record")
	}
	return rec, nil
}

// insertNonce generates a nonce and records it with its expiry. As with tokens, each attempt draws a fresh one.
func (db *database) insertNonce(ctx context.Context, expiresAt time.Time) ([]byte, error) {
	return retry.ExecuteWithResult(ctx, db.retry, func() ([]byte, error) {
		nonce, randErr := randomBytes()
		if randErr != nil {
			return nil, randErr
		}
		if _, execErr := db.pool.Exec(ctx, sqlInsertNonce, nonce, expiresAt.Unix()); execErr != nil {
			return nil, errors.Wrap(execErr, "failed to persist nonce")
		}
		return nonce, nil
	}, puddle.ErrClosedPool)
}

// consumeNonce redeems a nonce, returning errNonceNotFound unless it was present and unexpired.
func (db *database) consumeNonce(ctx context.Context, nonce []byte, now time.Time) error {
	if len(nonce) == 0 {
		return errors.Wrap(errNonceNotFound, "envelope carries no nonce")
	}
	consumed, err := db.execRowsAffected(ctx, sqlConsumeNonce, nonce, now.Unix())
	if err != nil {
		return err
	}
	if consumed == 0 {
		return errNonceNotFound
	}
	return nil
}

// deleteExpired deletes the token and nonce records that lapsed before now, so neither table grows without
// bound - clients may well request nonces they never redeem.
func (db *database) deleteExpired(ctx context.Context, now time.Time) (deletedTokens, deletedNonces int64, err error) {
	deletedTokens, err = db.execRowsAffected(ctx, sqlDeleteExpiredTokens, now.Unix())
	if err != nil {
		return 0, 0, err
	}
	deletedNonces, err = db.execRowsAffected(ctx, sqlDeleteExpiredNonces, now.Unix())
	return deletedTokens, deletedNonces, err
}

// execRowsAffected executes a statement until it succeeds or the retry profile gives up, and returns the
// number of rows it affected.
func (db *database) execRowsAffected(ctx context.Context, sqlStmt string, args ...any) (int64, error) {
	tag, err := retry.ExecuteWithResult(ctx, db.retry, func() (pgconn.CommandTag, error) {
		tag, execErr := db.pool.Exec(ctx, sqlStmt, args...)
		return tag, errors.Wrapf(execErr, "failed to execute the SQL statement [%s]", sqlStmt)
	}, puddle.ErrClosedPool)
	return tag.RowsAffected(), err
}

func randomBytes() ([]byte, error) {
	b := make([]byte, randomLength)
	if _, err := rand.Read(b); err != nil {
		return nil, errors.Wrap(err, "failed to generate random bytes")
	}
	return b, nil
}

// hashToken is the key a token's record is stored under, so the store holds nothing a client could present.
// A plain hash suffices: 256 random bits cannot be brute-forced.
func hashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}
