/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package runner

import (
	"net"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/cmd/config"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

// allocateServices allocates service configurations with default counts if not specified.
// Defaults: 3 orderers, 2 verifiers, 2 VC services, 1 each for other services.
func allocateServices(t *testing.T, conf *Config, credFactory *test.CredentialsFactory) config.SystemServices {
	t.Helper()
	s := serviceAllocator{credFactory: credFactory, tlsMode: conf.TLSMode}
	defer s.close()
	return config.SystemServices{
		Orderer:     s.allocateService(t, conf.NumOrderers),
		Verifier:    s.allocateService(t, conf.NumVerifiers),
		VCService:   s.allocateService(t, conf.NumVCService),
		Query:       s.allocateService(t, 1)[0],
		Coordinator: s.allocateService(t, 1)[0],
		Sidecar:     s.allocateService(t, 1)[0],
		LoadGen:     s.allocateService(t, 1)[0],
	}
}

type serviceAllocator struct {
	credFactory *test.CredentialsFactory
	tlsMode     string
	listeners   []net.Listener
}

// allocateService finds a range of available ports.
func (p *serviceAllocator) allocateService(t *testing.T, count int) []config.ServiceConfig {
	t.Helper()
	require.Positive(t, count)
	serviceConfigs := make([]config.ServiceConfig, count)
	for i := range serviceConfigs {
		conf := &serviceConfigs[i]
		conf.GrpcEndpoint = p.allocateEndpoint(t)
		conf.MetricsEndpoint = p.allocateEndpoint(t)
		conf.GrpcTLS, _ = p.credFactory.CreateServerCredentials(t, p.tlsMode, conf.GrpcEndpoint.Host)
		conf.MetricsTLS, _ = p.credFactory.CreateServerCredentials(t, p.tlsMode, conf.MetricsEndpoint.Host)
	}
	return serviceConfigs
}

func (p *serviceAllocator) allocateEndpoint(t *testing.T) *connection.Endpoint {
	t.Helper()
	s := connection.NewLocalHostServer(test.InsecureTLSConfig)
	listener, err := s.Listener(t.Context())
	require.NoError(t, err)
	p.listeners = append(p.listeners, listener)
	return &s.Endpoint
}

// close releases the ports to be used for their intended purpose.
func (p *serviceAllocator) close() {
	connection.CloseConnectionsLog(p.listeners...)
	p.listeners = nil
}
