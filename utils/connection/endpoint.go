/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package connection

import (
	"net"
	"strconv"
)

// DefaultHost is the default hostname used for service endpoints.
const DefaultHost = "localhost"

// Endpoint describes a remote endpoint.
type Endpoint struct {
	Host string `mapstructure:"host"`
	Port int    `mapstructure:"port"`
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
