/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package serve

import (
	"crypto/tls"
	"crypto/x509"
	"sync"
	"sync/atomic"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

type (
	// TLSProvider holds a dynamically updatable TLS configuration.
	//
	// The design separates writers (services) from readers (server), ensuring a linear dependency flow:
	//
	//	Service -> DynamicTLSUpdater <- TLSProvider <- Server
	TLSProvider struct {
		serverConfig        *tls.Config
		clientConfig        *tls.Config
		clientConfigVersion uint64
		staticCertPool      *x509.CertPool
		updater             *DynamicTLSUpdater
		mu                  sync.Mutex
	}

	// DynamicTLSUpdater is a TLS CA handler that can be used to update the GRPCTLSProvider.
	DynamicTLSUpdater struct {
		certs              atomic.Pointer[[][]byte]
		certsUpdateVersion atomic.Uint64
	}
)

// RegisterDynamicTLSUpdater registers a DynamicTLSUpdater with a GRPCTLSProvider.
// It uses the similar signature as GRPC server registration to allow common
// language for all server<->service interfaces.
func RegisterDynamicTLSUpdater(d *TLSProvider, updater *DynamicTLSUpdater) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.updater = updater
}

// NewTLSProvider loads TLS credentials from a TLSConfig.
func NewTLSProvider(tlsConfig connection.TLSConfig) (*TLSProvider, error) {
	creds, err := connection.NewServerTLSCredentials(tlsConfig)
	if err != nil {
		return nil, err
	}

	d := &TLSProvider{}
	d.clientConfig, err = creds.CreateServerTLSConfig()
	if err != nil {
		return nil, err
	}

	d.serverConfig = d.clientConfig
	if creds.Mode == connection.MutualTLSMode && d.clientConfig != nil {
		d.staticCertPool = d.clientConfig.ClientCAs
		d.serverConfig = &tls.Config{
			// tls.VersionTLS12 is the minimum version required to achieve secure connections.
			MinVersion:         tls.VersionTLS12,
			GetConfigForClient: d.GetConfigForClient,
		}
	}

	return d, nil
}

// GetServerTLSCredentials returns the TLS credentials for the server.
func (d *TLSProvider) GetServerTLSCredentials() *tls.Config {
	return d.serverConfig
}

// GetConfigForClient returns the current TLS config for a new client connection.
// This is a single atomic pointer load with no allocations, making it safe and
// efficient to call on every TLS handshake.
func (d *TLSProvider) GetConfigForClient(*tls.ClientHelloInfo) (*tls.Config, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.updateNoLock()
	return d.clientConfig, nil
}

func (d *TLSProvider) updateNoLock() bool {
	if d.updater == nil || d.clientConfig == nil || d.staticCertPool == nil {
		return false
	}

	// Loading the version before the certs ensures the loaded certificates are of version equal or higher
	// than the loaded version.
	// If the version is higher, we may update again on the next handshake.
	version := d.updater.certsUpdateVersion.Load()
	if version <= d.clientConfigVersion {
		return false
	}

	mergedPool := d.staticCertPool.Clone()
	// We ignore failed certificates here, and process the rest.
	// This ensures we allow partial updates to succeed even if some certs are invalid.
	// Otherwise, a single bad certificate could block access to the system to new clients,
	// or allow access to invalid clients.
	connection.ExtendCertPool(mergedPool, d.updater.Load()...)
	d.clientConfig.ClientCAs = mergedPool
	d.clientConfigVersion = version
	return true
}

// UpdateClientRootCAs updates the client root CAs with the given certificates.
func (d *DynamicTLSUpdater) UpdateClientRootCAs(certs [][]byte) {
	d.certs.Store(&certs)
	d.certsUpdateVersion.Add(1)
}

// Load loads the dynamic certificates.
func (d *DynamicTLSUpdater) Load() [][]byte {
	certs := d.certs.Load()
	if certs == nil {
		return nil
	}
	return *certs
}
