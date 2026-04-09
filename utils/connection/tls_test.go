/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package connection

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hyperledger/fabric-x-common/common/crypto/tlsgen"
	"github.com/stretchr/testify/require"
)

const localHost = "localhost"

type (
	testPaths struct {
		cert, key, ca string
	}

	expect struct {
		cert, key, ca bool
	}
)

// TestNewServerAndClientTLSCredentials verifies that TLSConfig can be loaded into TLSCredentials and converted
// to tls.Config with the correct certificates for each mode.
func TestNewServerAndClientTLSCredentials(t *testing.T) {
	t.Parallel()
	p := setupTestFiles(t)

	t.Run("server credentials", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name       string
			mode       string
			setupCfg   func(mode string) TLSConfig
			expectKeys expect
		}{
			{
				name:       "none-TLS",
				mode:       NoneTLSMode,
				setupCfg:   func(mode string) TLSConfig { return TLSConfig{Mode: mode} },
				expectKeys: expect{cert: false, key: false, ca: false},
			},
			{
				name: "server-side-TLS",
				mode: OneSideTLSMode,
				setupCfg: func(mode string) TLSConfig {
					return TLSConfig{Mode: mode, CertPath: p.cert, KeyPath: p.key}
				},
				expectKeys: expect{cert: true, key: true, ca: false},
			},
			{
				name: "mutual-TLS",
				mode: MutualTLSMode,
				setupCfg: func(mode string) TLSConfig {
					return TLSConfig{Mode: mode, CertPath: p.cert, KeyPath: p.key, CACertPaths: []string{p.ca}}
				},
				expectKeys: expect{cert: true, key: true, ca: true},
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				cfg := tc.setupCfg(tc.mode)

				m, err := NewServerTLSCredentials(cfg)
				require.NoError(t, err, "error while creating TLS credentials")
				requireCredentials(t, m, tc.expectKeys)

				_, err = m.CreateServerTLSConfig()
				require.NoError(t, err)
			})
		}
	})

	t.Run("client credentials", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name       string
			mode       string
			setupCfg   func(mode string) TLSConfig
			expectKeys expect
		}{
			{
				name:       "none-TLS",
				mode:       NoneTLSMode,
				setupCfg:   func(mode string) TLSConfig { return TLSConfig{Mode: mode} },
				expectKeys: expect{cert: false, key: false, ca: false},
			},
			{
				name: "server-side-TLS",
				mode: OneSideTLSMode,
				setupCfg: func(mode string) TLSConfig {
					return TLSConfig{Mode: mode, CACertPaths: []string{p.ca}}
				},
				expectKeys: expect{cert: false, key: false, ca: true},
			},
			{
				name: "mutual-TLS",
				mode: MutualTLSMode,
				setupCfg: func(mode string) TLSConfig {
					return TLSConfig{Mode: mode, CertPath: p.cert, KeyPath: p.key, CACertPaths: []string{p.ca}}
				},
				expectKeys: expect{cert: true, key: true, ca: true},
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				cfg := tc.setupCfg(tc.mode)

				m, err := NewClientTLSCredentials(cfg)
				require.NoError(t, err, "error while creating TLS credentials")
				requireCredentials(t, m, tc.expectKeys)

				_, err = m.CreateClientTLSConfig()
				require.NoError(t, err)
			})
		}
	})
}

func setupTestFiles(t *testing.T) testPaths {
	t.Helper()
	ca, err := tlsgen.NewCA()
	require.NoError(t, err)

	keyPair, err := ca.NewServerCertKeyPair(localHost)
	require.NoError(t, err)

	tmpDir := t.TempDir()
	paths := testPaths{
		cert: filepath.Join(tmpDir, "cert.pem"),
		key:  filepath.Join(tmpDir, "key.pem"),
		ca:   filepath.Join(tmpDir, "ca.pem"),
	}

	require.NoError(t, os.WriteFile(paths.cert, keyPair.Cert, 0o600))
	require.NoError(t, os.WriteFile(paths.key, keyPair.Key, 0o600))
	require.NoError(t, os.WriteFile(paths.ca, ca.CertBytes(), 0o600))

	return paths
}

func requireCredentials(t *testing.T, m *TLSCredentials, e expect) {
	t.Helper()
	require.Equal(t, e.cert, m.Cert != nil, "cert presence mismatch")
	require.Equal(t, e.key, m.Key != nil, "key presence mismatch")
	require.Equal(t, e.ca, m.CACerts != nil, "CA presence mismatch")
}
