/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package connection

import (
	"crypto/tls"
	"sync/atomic"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-lib-go/bccsp/factory"
	"github.com/hyperledger/fabric-x-common/common/channelconfig"
	"github.com/hyperledger/fabric-x-common/protoutil"
)

// TLSCertUpdater is the write-side interface for dynamic TLS CA rotation.
// Services (sidecar, query) use this to push updated root CA certificates
// when new organizations join or config blocks are received.
type TLSCertUpdater interface {
	SetClientRootCAs(certs [][]byte) error
}

// TLSConfigProvider is the read-side interface for dynamic TLS CA rotation.
// The gRPC server infrastructure uses this to obtain the current TLS config
// on each new client connection handshake.
type TLSConfigProvider interface {
	GetConfigForClient(*tls.ClientHelloInfo) (*tls.Config, error)
}

// DynamicTLS holds a dynamically updatable TLS configuration.
// It implements both TLSCertUpdater (for services to push CA updates)
// and TLSConfigProvider (for the server to read the current config).
//
// The design separates writers (services) from readers (server) through
// interface segregation, ensuring a linear dependency flow:
//
//	Service -> TLSCertUpdater -> DynamicTLS <- TLSConfigProvider <- Server
type DynamicTLS struct {
	config    atomic.Pointer[tls.Config]
	staticCAs [][]byte
}

// NewDynamicTLSFromConfig loads TLS credentials from a TLSConfig and creates a
// DynamicTLS. Callers should only invoke this for mutual TLS configurations.
func NewDynamicTLSFromConfig(tlsConfig TLSConfig) (*DynamicTLS, error) {
	creds, err := NewServerTLSCredentials(tlsConfig)
	if err != nil {
		return nil, err
	}

	baseCfg, err := creds.CreateServerTLSConfig()
	if err != nil {
		return nil, err
	}

	d := &DynamicTLS{staticCAs: creds.CACerts}
	d.config.Store(baseCfg)
	return d, nil
}

// SetClientRootCAs merges the provided dynamic CA certificates with the static CAs,
// builds a new certificate pool, and atomically swaps the stored TLS config.
// This is called by services when they receive updated CA certificates
// (e.g., from config blocks or database reads).
func (d *DynamicTLS) SetClientRootCAs(certs [][]byte) error {
	merged := make([][]byte, 0, len(d.staticCAs)+len(certs))
	merged = append(merged, d.staticCAs...)
	merged = append(merged, certs...)

	pool, err := buildCertPool(merged)
	if err != nil {
		return err
	}

	cfg := d.config.Load().Clone()
	cfg.ClientCAs = pool
	d.config.Store(cfg)
	return nil
}

// GetConfigForClient returns the current TLS config for a new client connection.
// This is a single atomic pointer load with no allocations, making it safe and
// efficient to call on every TLS handshake.
func (d *DynamicTLS) GetConfigForClient(*tls.ClientHelloInfo) (*tls.Config, error) {
	return d.config.Load(), nil
}

// ExtractAppTLSCAsFromEnvelope parses a Fabric config envelope and extracts
// TLS root CA certificates from application organizations.
// Only application org CAs are needed because the sidecar and query services
// accept connections from application clients, not orderer nodes.
func ExtractAppTLSCAsFromEnvelope(envelopeBytes []byte) ([][]byte, error) {
	envelope, err := protoutil.UnmarshalEnvelope(envelopeBytes)
	if err != nil {
		return nil, err
	}

	bundle, err := channelconfig.NewBundleFromEnvelope(envelope, factory.GetDefault())
	if err != nil {
		return nil, err
	}

	app, ok := bundle.ApplicationConfig()
	if !ok {
		return nil, errors.New("could not find application config in envelope")
	}

	var certs [][]byte
	for _, org := range app.Organizations() {
		certs = append(certs, org.MSP().GetTLSRootCerts()...)
	}

	return certs, nil
}
