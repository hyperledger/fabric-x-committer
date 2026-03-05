/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package ordererconn

import (
	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-lib-go/bccsp/factory"
	commontypes "github.com/hyperledger/fabric-x-common/api/types"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

type (
	// Config defines the static configuration of the orderer client as loaded from the YAML file.
	// It supports connectivity to multiple organization's orderers.
	Config struct {
		ConsensusType string                         `mapstructure:"consensus-type"`
		ChannelID     string                         `mapstructure:"channel-id"`
		Identity      *IdentityConfig                `mapstructure:"identity"`
		Retry         *connection.RetryProfile       `mapstructure:"reconnect"`
		TLS           OrdererTLSConfig               `mapstructure:"tls"`
		Organizations map[string]*OrganizationConfig `mapstructure:"organizations"`
	}

	// IdentityConfig defines the orderer's MSP.
	IdentityConfig struct {
		// MspID indicates to which MSP this client belongs to.
		MspID  string               `mapstructure:"msp-id" yaml:"msp-id"`
		MSPDir string               `mapstructure:"msp-dir" yaml:"msp-dir"`
		BCCSP  *factory.FactoryOpts `mapstructure:"bccsp" yaml:"bccsp"`
	}

	// OrganizationConfig contains the MspID (Organization ID), orderer endpoints, and their root CA paths.
	OrganizationConfig struct {
		Endpoints []*commontypes.OrdererEndpoint `mapstructure:"endpoints"`
		CACerts   []string                       `mapstructure:"ca-cert-paths"`
	}

	// OrdererTLSConfig is a TLS config for the orderer clients.
	OrdererTLSConfig struct {
		Mode     string `mapstructure:"mode"`
		CertPath string `mapstructure:"cert-path"`
		KeyPath  string `mapstructure:"key-path"`
	}
)

const (
	// Cft client support for crash fault tolerance.
	Cft = "CFT"
	// Bft client support for byzantine fault tolerance.
	Bft = "BFT"
	// DefaultConsensus default fault tolerance.
	DefaultConsensus = Cft

	// Broadcast support by endpoint.
	Broadcast = "broadcast"
	// Deliver support by endpoint.
	Deliver = "deliver"
)

// Errors that may be returned when updating a configuration.
var (
	ErrEmptyConnectionConfig = errors.New("empty connection config")
	ErrEmptyEndpoint         = errors.New("empty endpoint")
	ErrNoEndpoints           = errors.New("no endpoints")
)

// ValidateOrganizations validate the organization parameters.
func ValidateOrganizations(organizations ...*OrganizationMaterial) error {
	for _, org := range organizations {
		if org == nil {
			return ErrEmptyConnectionConfig
		}
		if err := validateEndpoints(org.Endpoints); err != nil {
			return err
		}
	}
	return nil
}

func validateEndpoints(endpoints []*commontypes.OrdererEndpoint) error {
	if len(endpoints) == 0 {
		return ErrNoEndpoints
	}
	uniqueEndpoints := make(map[string]string)
	for _, e := range endpoints {
		if e.Host == "" || e.Port == 0 {
			return ErrEmptyEndpoint
		}
		target := e.Address()
		if other, ok := uniqueEndpoints[target]; ok {
			return errors.Newf("endpoint [%s] specified multiple times: %s, %s", target, other, e.String())
		}
		uniqueEndpoints[target] = e.String()
	}
	return nil
}

// ValidateConsensusType verify and sets the consensus type in case of an unmentioned type.
func ValidateConsensusType(c *Config) error {
	if c.ConsensusType == "" {
		c.ConsensusType = DefaultConsensus
	}
	if c.ConsensusType != Bft && c.ConsensusType != Cft {
		return errors.Newf("unsupported orderer type %s", c.ConsensusType)
	}
	return nil
}

// TLSConfigToOrdererTLSConfig translates a TLSConfig to an OrdererTLSConfig.
func TLSConfigToOrdererTLSConfig(c connection.TLSConfig) OrdererTLSConfig {
	return OrdererTLSConfig{
		Mode:     c.Mode,
		KeyPath:  c.KeyPath,
		CertPath: c.CertPath,
	}
}
