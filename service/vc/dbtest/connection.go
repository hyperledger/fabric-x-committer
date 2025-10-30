/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package dbtest

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/yugabyte/pgx/v4/pgxpool"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/logging"
)

var logger = logging.New("db connection")

const (
	defaultUsername = "yugabyte"
	defaultPassword = "yugabyte"

	createDBSQLTempl = "CREATE DATABASE %s;"
	dropDBSQLTempl   = "DROP DATABASE IF EXISTS %s WITH (FORCE);"
)

// defaultRetry is used for tests.
var defaultRetry = &connection.RetryProfile{
	// MaxElapsedTime is the duration allocated for the retry mechanism during the database initialization process.
	MaxElapsedTime: 5 * time.Minute,
	// InitialInterval is the starting wait time interval that increases every retry attempt.
	InitialInterval: time.Duration(rand.Intn(900)+100) * time.Millisecond,
}

// Connection facilities connecting to a YugabyteDB instance.
type Connection struct {
	Endpoints   []*connection.Endpoint
	User        string
	Password    string
	Database    string
	LoadBalance bool
	TLS         connection.DatabaseTLSConfig
}

// NewConnection returns a connection parameters with the specified host:port, and the default values
// for the other parameters.
func NewConnection(endpoints ...*connection.Endpoint) *Connection {
	return &Connection{
		Endpoints: endpoints,
		User:      defaultUsername,
		Password:  defaultPassword,
	}
}

// dataSourceName returns the dataSourceName to be used by the database/sql package.
func (c *Connection) dataSourceName() (string, error) {
	ret := fmt.Sprintf("postgres://%s:%s@%s/%s?",
		c.User, c.Password, c.endpointsString(), c.Database)

	switch c.TLS.Mode {
	case connection.NoneTLSMode, connection.UnmentionedTLSMode:
		ret += "sslmode=disable"
	case connection.OneSideTLSMode:
		// Enforce full SSL verification:
		// requires an encrypted connection (TLS),
		// and ensures the server hostname matches the certificate.
		ret += "sslmode=verify-full"
		ret += fmt.Sprintf("&sslrootcert=%s", c.TLS.CACertPath)
	case connection.MutualTLSMode:
		return "", errors.Newf("unsupportted db tls mode: %s", c.TLS.Mode)
	default:
		return "", errors.Newf("unknown TLS mode: %s (valid modes: %s, %s, %s)",
			c.TLS.Mode, connection.NoneTLSMode, connection.OneSideTLSMode, connection.MutualTLSMode)
	}
	// The load balancing flag is only available when the server supports it (having multiple nodes).
	// Thus, we only add it when explicitly required. Otherwise, an error will occur.
	if c.LoadBalance {
		ret += "&load_balance=true"
	}
	return ret, nil
}

// endpointsString returns the address:port as a string with comma as a separator between endpoints.
func (c *Connection) endpointsString() string {
	return connection.AddressString(c.Endpoints...)
}

// open opens a connection pool to the database.
func (c *Connection) open(ctx context.Context) (*pgxpool.Pool, error) {
	connString, err := c.dataSourceName()
	if err != nil {
		return nil, errors.Wrapf(err, "could not build database connection string")
	}
	poolConfig, err := pgxpool.ParseConfig(connString)
	if err != nil {
		return nil, errors.Wrapf(err, "error parsing datasource: %s", c.endpointsString())
	}

	poolConfig.MaxConns = 1
	poolConfig.MinConns = 1

	var pool *pgxpool.Pool
	if retryErr := defaultRetry.Execute(ctx, func() error {
		pool, err = pgxpool.ConnectConfig(ctx, poolConfig)
		return err
	}); retryErr != nil {
		return nil, errors.Wrapf(err, "error making pool: %s", c.endpointsString())
	}
	return pool, nil
}

// waitForReady repeatably checks readiness until positive response arrives.
func (c *Connection) waitForReady(ctx context.Context) bool {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for !c.isEndpointReady(ctx) {
		select {
		case <-ctx.Done():
			// Stop trying if the context cancelled
			return false
		case <-ticker.C:
		}
	}

	return true
}

// isEndpointReady attempts to ping the database and returns true if successful.
func (c *Connection) isEndpointReady(ctx context.Context) bool {
	conn, err := c.open(ctx)
	if err != nil {
		logger.Debugf("[%s] error opening connection: %s", c.endpointsString(), err)
		return false
	}
	defer conn.Close()

	if err = conn.Ping(ctx); err != nil {
		logger.Debugf("[%s] error pinging connection: %s", c.endpointsString(), err)
		return false
	}
	logger.Infof("[%s] Connected to database", c.endpointsString())
	return true
}

func (c *Connection) execute(ctx context.Context, stmt string) error {
	pool, err := c.open(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()
	return defaultRetry.ExecuteSQL(ctx, pool, stmt)
}
