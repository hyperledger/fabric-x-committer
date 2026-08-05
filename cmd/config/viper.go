/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package config

import (
	"strings"

	"github.com/spf13/viper"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

var (
	envStringReplacer = strings.NewReplacer("-", "_", ".", "_")
	scEnvPrefix       = "SC_"
)

// Default per-service ports. Ports are the only defaults kept as constants: they differ per service
// (so a shared struct tag can't express them), while every other default is a `default:"..."` tag.
const (
	coordinatorServerPort     = 9001
	coordinatorMonitoringPort = 2119
	sidecarServerPort         = 4001
	sidecarMonitoringPort     = 2114
	verifierServerPort        = 5001
	verifierMonitoringPort    = 2115
	vcServerPort              = 6001
	vcMonitoringPort          = 2116
	queryServerPort           = 7001
	queryMonitoringPort       = 2117
	loadgenServerPort         = 8001
	loadgenMonitoringPort     = 2118
)

// NewViperWithCoordinatorDefaults returns a viper instance with the coordinator default values.
func NewViperWithCoordinatorDefaults() *viper.Viper {
	v := newViperWithServiceDefault("coordinator", coordinatorServerPort, coordinatorMonitoringPort)
	setEndpoint(v, "verifier.endpoints", verifierServerPort)
	setEndpoint(v, "validator-committer.endpoints", vcServerPort)
	return v
}

// NewViperWithSidecarDefaults returns a viper instance with the sidecar default values.
func NewViperWithSidecarDefaults() *viper.Viper {
	v := newViperWithServiceDefault("sidecar", sidecarServerPort, sidecarMonitoringPort)
	setEndpoint(v, "committer.endpoint", coordinatorServerPort)
	setClientFacingServerLimits(v)
	return v
}

// NewViperWithVerifierDefaults returns a viper instance with the verifier default values.
func NewViperWithVerifierDefaults() *viper.Viper {
	return newViperWithServiceDefault("verifier", verifierServerPort, verifierMonitoringPort)
}

// NewViperWithVCDefaults returns a viper instance with the VC default values.
func NewViperWithVCDefaults() *viper.Viper {
	return newViperWithServiceDefault("vc", vcServerPort, vcMonitoringPort)
}

// NewViperWithQueryDefaults returns a viper instance with the query-service default values.
func NewViperWithQueryDefaults() *viper.Viper {
	v := newViperWithServiceDefault("query", queryServerPort, queryMonitoringPort)
	setClientFacingServerLimits(v)
	return v
}

// NewViperWithLoadGenDefaults returns a viper instance with the load generator default values.
func NewViperWithLoadGenDefaults() *viper.Viper {
	return newViperWithServiceDefault("loadgen", loadgenServerPort, loadgenMonitoringPort)
}

// NewViperWithOrdererDefaults returns a viper instance with the mock-orderer service default values.
func NewViperWithOrdererDefaults() *viper.Viper {
	return NewViperWithLoggingDefault("orderer")
}

// newViperWithServiceDefault returns a viper instance with a service's default server and monitoring
// endpoints.
func newViperWithServiceDefault(serviceName string, servicePort, monitoringPort int) *viper.Viper {
	v := NewViperWithLoggingDefault(serviceName)
	setEndpoint(v, "server.endpoint", servicePort)
	setEndpoint(v, "monitoring.endpoint", monitoringPort)
	return v
}

// NewViperWithLoggingDefault returns a viper instance with the logging default values.
func NewViperWithLoggingDefault(serviceName string) *viper.Viper {
	v := viper.New()
	v.AutomaticEnv()
	v.SetEnvKeyReplacer(envStringReplacer)
	v.SetEnvPrefix(strings.ToUpper(scEnvPrefix + serviceName))
	return v
}

// setClientFacingServerLimits sets the gRPC rate-limit and max-concurrent-streams defaults for the
// client-facing services (query, sidecar); internal services are left unlimited.
func setClientFacingServerLimits(v *viper.Viper) {
	v.SetDefault("server.rate-limit.requests-per-second", 5_000)
	v.SetDefault("server.rate-limit.burst", 1_000)
	v.SetDefault("server.max-concurrent-streams", 10)
}

// setEndpoint registers a default "localhost:port" endpoint at the given viper key, decoded into a
// single endpoint or a one-element list depending on the target field.
func setEndpoint(v *viper.Viper, key string, port int) {
	v.SetDefault(key, (&connection.Endpoint{Host: connection.DefaultHost, Port: port}).String())
}
