/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"fmt"
	"path/filepath"

	"github.com/hyperledger/fabric-x-common/tools/cryptogen"
	"github.com/hyperledger/fabric-x-common/utils/connection"
)

var (
	// OrgRootCA is the path to organization 0's TLS client credentials in the crypto materials directory.
	OrgRootCA = filepath.Join(cryptogen.PeerOrganizationsDir, "peer-org-0.com",
		cryptogen.MSPDir, cryptogen.TLSCaCertsDir, "tlspeer-org-0-CA-cert.pem")

	// OrdererRootCATLSPath is the path to organization 0's orderer TLS credentials in the crypto materials directory.
	OrdererRootCATLSPath = filepath.Join(cryptogen.OrdererOrganizationsDir,
		"orderer-org-0.com", cryptogen.MSPDir, cryptogen.TLSCaCertsDir, "tlsorderer-org-0-CA-cert.pem")
)

// NewServiceTLSConfig creates a server TLS configuration with certificates loaded from the artifact path.
// This function constructs paths to TLS certificates for a given service within the peer organization structure.
func NewServiceTLSConfig(artifactsPath, serviceName, mode string) connection.TLSConfig {
	subPath := filepath.Join(artifactsPath, cryptogen.PeerOrganizationsDir, "peer-org-0.com",
		cryptogen.PeerNodesDir, serviceName, cryptogen.TLSDir)
	return connection.TLSConfig{
		Mode:     mode,
		CertPath: filepath.Join(subPath, "server.crt"),
		KeyPath:  filepath.Join(subPath, "server.key"),
		CACertPaths: []string{
			filepath.Join(artifactsPath, OrgRootCA),
		},
	}
}

// OrgClientTLSConfig creates a mutual TLS client configuration using a specific
// peer organization's TLS client certificate. The serverCACertPaths are the CA
// certs needed to verify the server (typically from the CredentialsFactory).
func OrgClientTLSConfig(artifactsPath string, orgIndex int, serverCACertPaths []string) connection.TLSConfig {
	orgDomain := fmt.Sprintf("peer-org-%d.com", orgIndex)
	tlsDir := filepath.Join(artifactsPath, cryptogen.PeerOrganizationsDir, orgDomain,
		cryptogen.UsersDir, fmt.Sprintf("client@%s", orgDomain), cryptogen.TLSDir)
	return connection.TLSConfig{
		Mode:        connection.MutualTLSMode,
		CertPath:    filepath.Join(tlsDir, "client.crt"),
		KeyPath:     filepath.Join(tlsDir, "client.key"),
		CACertPaths: serverCACertPaths,
	}
}
