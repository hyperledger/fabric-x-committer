/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package broadcastdeliver

import (
	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-lib-go/bccsp/factory"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

type (
	// Config for the orderer-client.
	Config struct {
		Connection    ConnectionConfig `mapstructure:"connection"`
		ConsensusType ConsensusType    `mapstructure:"consensus-type"`
		ChannelID     string           `mapstructure:"channel-id"`
		Identity      *IdentityConfig  `mapstructure:"identity"`
	}

	// ConnectionConfig contains the endpoints, CAs, and retry profile.
	ConnectionConfig struct {
		Endpoints []*connection.OrdererEndpoint `mapstructure:"endpoints"`
		Creds     *connection.TLSConfig         `mapstructure:"tls-config"`
		Retry     *connection.RetryProfile      `mapstructure:"reconnect"`
	}

	// IdentityConfig defines the orderer's MSP.
	IdentityConfig struct {
		SignedEnvelopes bool `mapstructure:"signed-envelopes" yaml:"signed-envelopes"`
		// MspID indicates to which MSP this client belongs to.
		MspID  string               `mapstructure:"msp-id" yaml:"msp-id"`
		MSPDir string               `mapstructure:"msp-dir" yaml:"msp-dir"`
		BCCSP  *factory.FactoryOpts `mapstructure:"bccsp" yaml:"bccsp"`
	}

	// ConsensusType can be either CFT or BFT.
	ConsensusType = string
)

const (
	// Cft client support for crash fault tolerance.
	Cft ConsensusType = "CFT"
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

func validateConfig(c *Config) error {
	if c.ConsensusType == "" {
		c.ConsensusType = DefaultConsensus
	}
	if c.ConsensusType != Bft && c.ConsensusType != Cft {
		return errors.Newf("unsupported orderer type %s", c.ConsensusType)
	}
	return validateConnectionConfig(&c.Connection)
}

func validateConnectionConfig(c *ConnectionConfig) error {
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
