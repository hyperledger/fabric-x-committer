/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package testcrypto

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-x-common/msp"
	"github.com/hyperledger/fabric-x-common/tools/cryptogen"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/ordererdial"
	"github.com/hyperledger/fabric-x-committer/utils/retry"
)

// GetPeersIdentities returns the peers' identities from a crypto path.
func GetPeersIdentities(cryptoPath string) ([]msp.SigningIdentity, error) {
	return GetSigningIdentities(GetPeersMspDirs(cryptoPath)...)
}

// GetConsenterIdentities returns the orderer consenters identities from a crypto path.
func GetConsenterIdentities(cryptoPath string) ([]msp.SigningIdentity, error) {
	return GetSigningIdentities(GetOrdererMspDirs(cryptoPath)...)
}

// GetSigningIdentities loads signing identities from the given MSP directories.
func GetSigningIdentities(mspDirs ...*msp.DirLoadParameters) ([]msp.SigningIdentity, error) {
	identities := make([]msp.SigningIdentity, len(mspDirs))
	for i, mspDir := range mspDirs {
		localMsp, err := msp.LoadLocalMspDir(*mspDir)
		if err != nil {
			return nil, err
		}
		identities[i], err = localMsp.GetDefaultSigningIdentity()
		if err != nil {
			return nil, errors.Wrap(err, "loading signing identity")
		}
	}
	return identities, nil
}

// GetPeersMspDirs returns the peers' MSP directory path.
// It discovers the client user directory by scanning the users/ directory
// for an entry matching "client@*", rather than assuming a specific domain suffix.
func GetPeersMspDirs(cryptoPath string) []*msp.DirLoadParameters {
	peerOrgPath := path.Join(cryptoPath, cryptogen.PeerOrganizationsDir)
	peerMspDirs := GetMspDirs(peerOrgPath)
	for _, mspItem := range peerMspDirs {
		usersDir := path.Join(mspItem.MspDir, "users")
		entries, _ := os.ReadDir(usersDir)
		for _, entry := range entries {
			if entry.IsDir() && strings.HasPrefix(entry.Name(), "client@") {
				mspItem.MspDir = path.Join(usersDir, entry.Name(), "msp")
				break
			}
		}
	}
	return peerMspDirs
}

// GetOrdererMspDirs returns the orderers' MSP directory path.
func GetOrdererMspDirs(cryptoPath string) []*msp.DirLoadParameters {
	ordererOrgPath := path.Join(cryptoPath, cryptogen.OrdererOrganizationsDir)
	ordererMspDirs := GetMspDirs(ordererOrgPath)
	for _, mspItem := range ordererMspDirs {
		nodeName := "consenter-" + mspItem.MspName[len("orderer-"):]
		mspItem.MspDir = path.Join(mspItem.MspDir, "orderers", nodeName, "msp")
	}
	return ordererMspDirs
}

// GetMspDirs returns the MSP dir parameter per organization in the path.
func GetMspDirs(targetPath string) []*msp.DirLoadParameters {
	dir, err := os.ReadDir(targetPath)
	if err != nil {
		return nil
	}
	mspDirs := make([]*msp.DirLoadParameters, 0, len(dir))
	for _, dirEntry := range dir {
		if !dirEntry.IsDir() {
			continue
		}
		orgName := dirEntry.Name()
		mspDirs = append(mspDirs, &msp.DirLoadParameters{
			MspName: orgName,
			MspDir:  path.Join(targetPath, orgName),
		})
	}
	return mspDirs
}

// GetOrdererConnConfig returns the configuration for an orderer connection using the config block and peer
// organizations in tha artifacts path.
func GetOrdererConnConfig(artifactsPath string, clientTLSConfig connection.TLSConfig) ordererdial.Config {
	peerMsp := GetPeersMspDirs(artifactsPath)
	var id *ordererdial.IdentityConfig
	if len(peerMsp) > 0 {
		id = &ordererdial.IdentityConfig{
			MspID:  peerMsp[0].MspName,
			MSPDir: peerMsp[0].MspDir,
			BCCSP:  peerMsp[0].CspConf,
		}
	}
	return ordererdial.Config{
		FaultToleranceLevel:        ordererdial.BFT,
		TLS:                        ordererdial.TLSConfigToOrdererTLSConfig(clientTLSConfig),
		LatestKnownConfigBlockPath: path.Join(artifactsPath, cryptogen.ConfigBlockFileName),
		Retry: &retry.Profile{
			InitialInterval: 10 * time.Millisecond,
			MaxInterval:     100 * time.Millisecond,
			Multiplier:      2,
			MaxElapsedTime:  time.Second,
		},
		Identity:                     id,
		SuspicionGracePeriodPerBlock: time.Second,
	}
}

// OrgClientTLSConfig creates a mutual TLS client configuration using a specific
// peer organization's TLS client certificate. The serverCACertPaths are the CA
// certs needed to verify the server (typically from the CredentialsFactory).
func OrgClientTLSConfig(artifactsPath string, orgIndex int, serverCACertPaths []string) connection.TLSConfig {
	orgName := fmt.Sprintf("peer-org-%d", orgIndex)
	orgDomain := fmt.Sprintf("peer-org-%d.com", orgIndex)
	tlsDir := filepath.Join(artifactsPath, cryptogen.PeerOrganizationsDir, orgName,
		cryptogen.UsersDir, fmt.Sprintf("client@%s", orgDomain), cryptogen.TLSDir)
	return connection.TLSConfig{
		Mode:        connection.MutualTLSMode,
		CertPath:    filepath.Join(tlsDir, "client.crt"),
		KeyPath:     filepath.Join(tlsDir, "client.key"),
		CACertPaths: serverCACertPaths,
	}
}
