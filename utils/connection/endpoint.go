/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package connection

import (
	"net"
	"strconv"

	"github.com/cockroachdb/errors"
)

// Endpoint describes a remote endpoint.
type Endpoint struct {
	Host string `mapstructure:"host" json:"host,omitempty" yaml:"host,omitempty"`
	Port int    `mapstructure:"port" json:"port,omitempty" yaml:"port,omitempty"`
}

// Empty returns true if no host and no port are assigned.
func (e *Endpoint) Empty() bool {
	return e.Host == "" && e.Port == 0
}

// Address returns a string representation of the endpoint's address.
func (e *Endpoint) Address() string {
	return net.JoinHostPort(e.Host, strconv.Itoa(e.Port))
}

// String returns a string representation of the endpoint.
func (e *Endpoint) String() string {
	return e.Address()
}

// GetHost returns the host of the endpoint.
func (e *Endpoint) GetHost() string { return e.Host }

// CreateEndpointHP parses an endpoint from give host and port.
// It panics if it fails to parse.
func CreateEndpointHP(host, port string) *Endpoint {
	convertedPort, err := strconv.Atoi(port)
	if err != nil {
		panic(errors.New("could not convert port to integer"))
	}
	return &Endpoint{
		Host: host,
		Port: convertedPort,
	}
}

// NewEndpoint parses an endpoint from an address string.
func NewEndpoint(hostPort string) (*Endpoint, error) {
	if len(hostPort) == 0 {
		return &Endpoint{}, nil
	}
	host, port, err := net.SplitHostPort(hostPort)
	if err != nil {
		return nil, errors.Wrap(err, "could not split host and port")
	}
	portInt, err := strconv.Atoi(port)
	if err != nil {
		return nil, errors.Wrap(err, "could not convert port to integer")
	}
	return &Endpoint{Host: host, Port: portInt}, nil
}

// NewLocalHost returns a default endpoint "localhost:0".
func NewLocalHost() *Endpoint {
	return &Endpoint{Host: "127.0.0.1"}
}
