/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package connection

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"github.com/hyperledger/fabric-x-common/api/types"
	"github.com/hyperledger/fabric-x-common/common/crypto/tlsgen"
	"github.com/hyperledger/fabric-x-common/tools/configtxgen"
	"github.com/hyperledger/fabric-x-common/tools/cryptogen"
	"github.com/stretchr/testify/require"
)

func TestDynamicTLS(t *testing.T) {
	t.Parallel()

	var cas [3]tlsgen.CA
	for i := range cas {
		ca, err := tlsgen.NewCA()
		require.NoError(t, err)
		cas[i] = ca
	}

	for _, tc := range []struct {
		name       string
		updates    []tlsgen.CA // sequential SetClientRootCAs calls
		contain    []tlsgen.CA
		notContain []tlsgen.CA
	}{
		{
			name:       "initial config has only static CAs",
			contain:    []tlsgen.CA{cas[0]},
			notContain: []tlsgen.CA{cas[1]},
		},
		{
			name:       "after SetClientRootCAs includes dynamic CAs",
			updates:    []tlsgen.CA{cas[1]},
			contain:    []tlsgen.CA{cas[0], cas[1]},
			notContain: []tlsgen.CA{cas[2]},
		},
		{
			name:       "SetClientRootCAs replaces previous dynamic CAs",
			updates:    []tlsgen.CA{cas[1], cas[2]},
			contain:    []tlsgen.CA{cas[0], cas[2]},
			notContain: []tlsgen.CA{cas[1]},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dtls := newTestDynamicTLS(t, cas[0])
			for _, ca := range tc.updates {
				require.NoError(t, dtls.SetClientRootCAs([][]byte{ca.CertBytes()}))
			}

			cfg, err := dtls.GetConfigForClient(nil)
			require.NoError(t, err)
			require.NotNil(t, cfg)
			requireCAs(t, cfg.ClientCAs, tc.contain, tc.notContain)
		})
	}

	t.Run("server certificate is preserved across updates", func(t *testing.T) {
		t.Parallel()
		dtls := newTestDynamicTLS(t, cas[0])
		require.NoError(t, dtls.SetClientRootCAs([][]byte{cas[1].CertBytes()}))

		cfg, err := dtls.GetConfigForClient(nil)
		require.NoError(t, err)
		require.Len(t, cfg.Certificates, 1)
	})

	t.Run("SetClientRootCAs rejects invalid cert", func(t *testing.T) {
		t.Parallel()
		dtls := newTestDynamicTLS(t, cas[0])
		require.Error(t, dtls.SetClientRootCAs([][]byte{[]byte("not a valid cert")}))
	})
}

func TestExtractAppTLSCAsFromEnvelope(t *testing.T) {
	t.Parallel()
	targetPath := t.TempDir()

	block, err := cryptogen.CreateOrExtendConfigBlockWithCrypto(cryptogen.ConfigBlockParameters{
		TargetPath:  targetPath,
		BaseProfile: configtxgen.SampleFabricX,
		ChannelID:   "test-channel",
		Organizations: []cryptogen.OrganizationParameters{
			{
				Name:   "orderer-org",
				Domain: "orderer-org.com",
				OrdererEndpoints: []*types.OrdererEndpoint{
					{Host: "localhost", Port: 7050},
				},
				ConsenterNodes: []cryptogen.Node{
					{CommonName: "consenter", Hostname: "consenter.com"},
				},
				OrdererNodes: []cryptogen.Node{
					{CommonName: "orderer", Hostname: "orderer.com", SANS: []string{"localhost"}},
				},
			},
			{
				Name:   "peer-org",
				Domain: "peer-org.com",
				PeerNodes: []cryptogen.Node{
					{CommonName: "peer0", Hostname: "peer0.com", SANS: []string{"localhost"}},
				},
			},
		},
	})
	require.NoError(t, err)
	require.NotEmpty(t, block.Data.Data)

	t.Run("extracts TLS CAs from valid config envelope", func(t *testing.T) {
		t.Parallel()
		certs, err := ExtractAppTLSCAsFromEnvelope(block.Data.Data[0])
		require.NoError(t, err)
		require.NotEmpty(t, certs, "should extract at least one TLS CA certificate")

		for _, cert := range certs {
			require.NotEmpty(t, cert)
			blk, _ := pem.Decode(cert)
			require.NotNil(t, blk, "each cert should be valid PEM")
		}
	})

	t.Run("returns error for invalid envelope", func(t *testing.T) {
		t.Parallel()
		_, err := ExtractAppTLSCAsFromEnvelope([]byte("invalid"))
		require.Error(t, err)
	})

	t.Run("returns error for nil envelope", func(t *testing.T) {
		t.Parallel()
		_, err := ExtractAppTLSCAsFromEnvelope(nil)
		require.Error(t, err)
	})
}

// newTestDynamicTLS creates a DynamicTLS via NewDynamicTLSFromConfig using the given CA
// for both server credentials and client CA trust.
func newTestDynamicTLS(t *testing.T, ca tlsgen.CA) *DynamicTLS {
	t.Helper()
	keyPair, err := ca.NewServerCertKeyPair(localHost)
	require.NoError(t, err)

	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	caPath := filepath.Join(dir, "ca.pem")
	require.NoError(t, os.WriteFile(certPath, keyPair.Cert, 0o600))
	require.NoError(t, os.WriteFile(keyPath, keyPair.Key, 0o600))
	require.NoError(t, os.WriteFile(caPath, ca.CertBytes(), 0o600))

	dtls, err := NewDynamicTLSFromConfig(TLSConfig{
		Mode:        MutualTLSMode,
		CertPath:    certPath,
		KeyPath:     keyPath,
		CACertPaths: []string{caPath},
	})
	require.NoError(t, err)
	return dtls
}

// requireCAs asserts that the pool contains all CAs in `contain` and none of the CAs in `notContain`.
func requireCAs(t *testing.T, pool *x509.CertPool, contain, notContain []tlsgen.CA) {
	t.Helper()
	require.NotNil(t, pool)

	for _, ca := range contain {
		cert := parsePEMCert(t, ca.CertBytes())
		_, err := cert.Verify(x509.VerifyOptions{Roots: pool})
		require.NoError(t, err, "pool should contain CA certificate")
	}

	for _, ca := range notContain {
		cert := parsePEMCert(t, ca.CertBytes())
		_, err := cert.Verify(x509.VerifyOptions{Roots: pool})
		require.Error(t, err, "pool should NOT contain CA certificate")
	}
}

func parsePEMCert(t *testing.T, pemBytes []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(pemBytes)
	require.NotNil(t, block, "failed to decode PEM block")

	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	return cert
}
