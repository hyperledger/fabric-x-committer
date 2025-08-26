/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package ordererconn

import (
	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-lib-go/bccsp/factory"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

type (
	// Config for the orderer-client.
	Config struct {
		Connection    ConnectionConfig `mapstructure:"connection"`
		ConsensusType string           `mapstructure:"consensus-type"`
		ChannelID     string           `mapstructure:"channel-id"`
		Identity      *IdentityConfig  `mapstructure:"identity"`
	}

	// ConnectionConfig contains the endpoints, CAs, and retry profile.
	ConnectionConfig struct {
		Endpoints []*Endpoint              `mapstructure:"endpoints"`
		TLS       *connection.TLSConfig    `mapstructure:"tls"`
		Retry     *connection.RetryProfile `mapstructure:"reconnect"`
	}

	// IdentityConfig defines the orderer's MSP.
	IdentityConfig struct {
		// MspID indicates to which MSP this client belongs to.
		MspID  string               `mapstructure:"msp-id" yaml:"msp-id"`
		MSPDir string               `mapstructure:"msp-dir" yaml:"msp-dir"`
		BCCSP  *factory.FactoryOpts `mapstructure:"bccsp" yaml:"bccsp"`
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

// ValidateConfig validate the configuration.
func ValidateConfig(c *Config) error {
	if c.ConsensusType == "" {
		c.ConsensusType = DefaultConsensus
	}
	if c.ConsensusType != Bft && c.ConsensusType != Cft {
		return errors.Newf("unsupported orderer type %s", c.ConsensusType)
	}
	return ValidateConnectionConfig(&c.Connection)
}

// ValidateConnectionConfig validate the configuration.
func ValidateConnectionConfig(c *ConnectionConfig) error {
	if c == nil {
		return ErrEmptyConnectionConfig
	}
	if len(c.Endpoints) == 0 {
		return ErrNoEndpoints
	}
	uniqueEndpoints := make(map[string]string)
	for _, e := range c.Endpoints {
		if e.Empty() {
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
