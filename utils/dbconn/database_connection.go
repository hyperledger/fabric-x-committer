/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package dbconn

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/yugabyte/pgx/v5/pgxpool"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

type (
	// DatabaseTLSConfig holds the database TLS mode and its necessary credentials.
	DatabaseTLSConfig struct {
		Mode       string `mapstructure:"mode"`
		CACertPath string `mapstructure:"ca-cert-path"`
	}

	// DataSourceNameParams defines the parameters required to construct a database connection string.
	DataSourceNameParams struct {
		Username        string
		Password        string
		Database        string
		EndpointsString string
		LoadBalance     bool
		TLS             DatabaseTLSConfig
	}

	// deadlineConn wraps a net.Conn with a per-read deadline so that reads
	// on dead connections fail instead of hanging indefinitely (on MacOS's Docker VM).
	deadlineConn struct {
		net.Conn
		readTimeout time.Duration
	}
)

// DataSourceName returns the data source name of the database.
func DataSourceName(d DataSourceNameParams) (string, error) {
	ret := fmt.Sprintf("postgres://%s:%s@%s/%s?",
		d.Username, d.Password, d.EndpointsString, d.Database)

	switch d.TLS.Mode {
	case connection.NoneTLSMode, connection.UnmentionedTLSMode:
		ret += "sslmode=disable"
	case connection.OneSideTLSMode:
		// Enforce full SSL verification:
		// requires an encrypted connection (TLS),
		// and ensures the server hostname matches the certificate.
		ret += fmt.Sprintf("sslmode=verify-full&sslrootcert=%s", d.TLS.CACertPath)
	case connection.MutualTLSMode:
		return "", errors.Newf("unsupportted db tls mode: %s", d.TLS.Mode)
	default:
		return "", errors.Newf("unknown TLS mode: %s (valid modes: %s, %s, %s)",
			d.TLS.Mode, connection.NoneTLSMode, connection.OneSideTLSMode, connection.MutualTLSMode)
	}
	// The load balancing flag is only available when the server supports it (having multiple nodes).
	// Thus, we only add it when explicitly required. Otherwise, an error will occur.
	if d.LoadBalance {
		ret += "&load_balance=true"
	}
	return ret, nil
}

// ConfigureConnReadDeadline configures pool connections with a per-read
// deadline so that reads on dead connections fail instead of hanging
// indefinitely.
//
// We use a custom DialFunc that wraps every net.Conn with deadlineConn,
// which calls SetReadDeadline before each Read. Pool callbacks like
// AfterConnect or BeforeAcquire only fire once, so they can only set a
// single absolute deadline that expires even if the connection is healthy.
// Wrapping net.Conn lets us reset the deadline on every Read call.
//
// The read timeout is set to 60 seconds, which is more than enough for
// read-set validation and commit operations. This can be made configurable
// if needed.
//
// On macOS, Docker Desktop runs a userspace TCP proxy inside a Linux VM.
// When a container dies the proxy does not immediately close forwarded
// connections, so reads hang indefinitely. The deadlineConn wrapper ensures
// stuck reads fail within the timeout, allowing the pool to replace them.
// On Linux, reads already fail immediately via TCP RST when a node dies,
// so the wrapper is harmless.
func ConfigureConnReadDeadline(poolConfig *pgxpool.Config) {
	dialer := &net.Dialer{
		KeepAlive: 3 * time.Second,
		Timeout:   10 * time.Second,
	}
	poolConfig.ConnConfig.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dialer.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &deadlineConn{Conn: conn, readTimeout: 60 * time.Second}, nil
	}
	// Lower from the default (1 min) so the pool detects dead idle
	// connections faster.
	poolConfig.HealthCheckPeriod = 30 * time.Second
}

func (c *deadlineConn) Read(b []byte) (int, error) {
	if err := c.SetReadDeadline(time.Now().Add(c.readTimeout)); err != nil {
		return 0, err
	}
	return c.Conn.Read(b)
}
