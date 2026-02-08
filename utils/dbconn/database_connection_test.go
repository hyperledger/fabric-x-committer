/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package dbconn

import (
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yugabyte/pgx/v5/pgxpool"

	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

func TestDataSourceName(t *testing.T) {
	t.Parallel()

	params := DataSourceNameParams{
		Username:        "user",
		Password:        "pass",
		Database:        "mydb",
		EndpointsString: "localhost:5433",
	}

	tests := []struct {
		name        string
		modify      func(*DataSourceNameParams)
		expectedDSN string
		expectedErr string
	}{
		{
			name:        "none TLS mode",
			modify:      func(p *DataSourceNameParams) { p.TLS.Mode = connection.NoneTLSMode },
			expectedDSN: "postgres://user:pass@localhost:5433/mydb?sslmode=disable",
		},
		{
			name:        "unmentioned TLS mode",
			modify:      func(p *DataSourceNameParams) { p.TLS.Mode = connection.UnmentionedTLSMode },
			expectedDSN: "postgres://user:pass@localhost:5433/mydb?sslmode=disable",
		},
		{
			name: "one-side TLS mode",
			modify: func(p *DataSourceNameParams) {
				p.TLS.Mode = connection.OneSideTLSMode
				p.TLS.CACertPath = "/path/to/ca.pem"
			},
			expectedDSN: "postgres://user:pass@localhost:5433/mydb?sslmode=verify-full&sslrootcert=/path/to/ca.pem",
		},
		{
			name:        "mutual TLS mode returns error",
			modify:      func(p *DataSourceNameParams) { p.TLS.Mode = connection.MutualTLSMode },
			expectedErr: "unsupportted db tls mode",
		},
		{
			name:        "unknown TLS mode returns error",
			modify:      func(p *DataSourceNameParams) { p.TLS.Mode = "bogus" },
			expectedErr: "unknown TLS mode",
		},
		{
			name: "load balance enabled",
			modify: func(p *DataSourceNameParams) {
				p.TLS.Mode = connection.NoneTLSMode
				p.LoadBalance = true
			},
			expectedDSN: "postgres://user:pass@localhost:5433/mydb?sslmode=disable&load_balance=true",
		},
		{
			name:        "load balance disabled",
			modify:      func(p *DataSourceNameParams) { p.TLS.Mode = connection.NoneTLSMode },
			expectedDSN: "postgres://user:pass@localhost:5433/mydb?sslmode=disable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			p := params
			tt.modify(&p)

			dsn, err := DataSourceName(p)

			if tt.expectedErr != "" {
				require.ErrorContains(t, err, tt.expectedErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.expectedDSN, dsn)
		})
	}
}

func TestConfigureConnReadDeadline(t *testing.T) {
	t.Parallel()

	poolConfig, err := pgxpool.ParseConfig("postgres://user:pass@localhost:5433/mydb")
	require.NoError(t, err)

	ConfigureConnReadDeadline(poolConfig)

	assert.Equal(t, 30*time.Second, poolConfig.HealthCheckPeriod)
	assert.NotNil(t, poolConfig.ConnConfig.DialFunc)
}

func TestDeadlineConn(t *testing.T) {
	t.Parallel()

	t.Run("successful read resets deadline each time", func(t *testing.T) {
		t.Parallel()

		server, client := tcpPair(t)

		dc := &deadlineConn{Conn: client, readTimeout: 5 * time.Second}

		expected := []byte("hello")
		_, err := server.Write(expected)
		require.NoError(t, err)

		buf := make([]byte, len(expected))
		n, err := dc.Read(buf)
		require.NoError(t, err)
		assert.Equal(t, expected, buf[:n])
	})

	t.Run("read times out on stalled connection", func(t *testing.T) {
		t.Parallel()

		_, client := tcpPair(t)

		dc := &deadlineConn{Conn: client, readTimeout: 50 * time.Millisecond}

		buf := make([]byte, 64)
		_, err := dc.Read(buf)
		require.Error(t, err)

		var netErr net.Error
		require.ErrorAs(t, err, &netErr)
		assert.True(t, netErr.Timeout())
	})
}

// tcpPair creates a connected pair of TCP connections for testing.
func tcpPair(t *testing.T) (server, client net.Conn) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	accepted := channel.NewReaderWriter(t.Context(), make(chan net.Conn, 1))
	go func() {
		conn, errA := ln.Accept()
		if errA == nil {
			accepted.Write(conn)
		}
	}()

	client, err = net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	server, _ = accepted.Read()
	t.Cleanup(func() { _ = server.Close() })

	return server, client
}
