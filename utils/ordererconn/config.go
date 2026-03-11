/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package ordererconn

import (
	"strings"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-lib-go/bccsp/factory"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

type (
	// Config defines the static configuration of the orderer as loaded from the YAML file.
	Config struct {
		FaultToleranceLevel string                   `mapstructure:"fault-tolerance-level"`
		TLS                 OrdererTLSConfig         `mapstructure:"tls"`
		Retry               *connection.RetryProfile `mapstructure:"reconnect"`
		// LastKnownConfigBlockPath is the path for the last known config block.
		// We fetch the orderer endpoints, CA certificates, and channel-ID from this block.
		// This block might be newer than the block used for verification, to allow using
		// the most up-to-date orderer connection data.
		LastKnownConfigBlockPath string `mapstructure:"last-known-config-block-path"`

		// The following parameters only applies to delivery.
		Identity                *IdentityConfig `mapstructure:"identity"`
		BlockWithholdingTimeout time.Duration   `mapstructure:"block-withholding-timeout"`
		SuspicionGracePeriod    time.Duration   `mapstructure:"suspicion-grace-period"`
		LivenessCheckInterval   time.Duration   `mapstructure:"liveness-check-interval"`
		MaxBlocksAhead          uint64          `mapstructure:"max-blocks-ahead"`
	}

	// IdentityConfig defines the orderer's client MSP.
	IdentityConfig struct {
		// MspID indicates to which MSP this client belongs to.
		MspID  string               `mapstructure:"msp-id" yaml:"msp-id"`
		MSPDir string               `mapstructure:"msp-dir" yaml:"msp-dir"`
		BCCSP  *factory.FactoryOpts `mapstructure:"bccsp" yaml:"bccsp"`
	}

	// OrdererTLSConfig is a TLS config for the orderer clients.
	OrdererTLSConfig struct {
		Mode     string `mapstructure:"mode"`
		CertPath string `mapstructure:"cert-path"`
		KeyPath  string `mapstructure:"key-path"`
		// CommonCACertPaths is a temporary workaround to inject CA to all organizations.
		// TODO: This will be removed once we read the TLS certificates from the config block.
		CommonCACertPaths []string `mapstructure:"common-ca-cert-paths"`
	}
)

// Fault tolerance levels.
// BFT (byzantine fault tolerance):
//   - For delivery: verifies blocks and monitors for block withholding.
//   - For broadcast: submit transactions to multiple orderers and waits for acknowledgments from a quorum.
//
// CFT (crash fault tolerance):
//   - For delivery: verifies blocks.
//   - For broadcast: submits transactions to a single orderer.
//
// UnspecifiedFT (empty string) defaults to the DefaultFT, which is the highest fault tolerance level (BFT).
const (
	UnspecifiedFT = ""
	BFT           = "BFT"
	CFT           = "CFT"
	DefaultFT     = BFT
)

// GetFaultToleranceLevel verify and returns the fault tolerance level.
func GetFaultToleranceLevel(ftLevel string) (string, error) {
	ftLevel = strings.ToUpper(ftLevel)
	switch ftLevel {
	case BFT, CFT:
		return ftLevel, nil
	case UnspecifiedFT:
		return DefaultFT, nil
	default:
		return ftLevel, errors.Newf("invalid fault tolerance level: '%s'", ftLevel)
	}
}
