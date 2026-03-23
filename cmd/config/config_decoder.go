/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package config

import (
	"math"
	"net"
	"reflect"
	"regexp"
	"strconv"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-x-common/common/viperutil"
	"github.com/mitchellh/mapstructure"
	"github.com/spf13/viper"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

// RFC 1123 hostname validation regex:
// - Labels (hostname components) can contain letters, digits, and hyphens.
// - Labels must start and end with alphanumeric.
// - Labels can be up to 63 characters.
var hostNameRegex = regexp.MustCompile(
	`(?i)^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)*[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`,
)

const (
	// Total hostname can be up to 253 characters (RFC 1123).
	hostNameMaxLength = 253
	// Maximal valid port number.
	maxPort = math.MaxUint16
)

// decoderHook contains custom unmarshalling for types not supported by default by mapstructure.
func decoderHook() viper.DecoderConfigOption {
	return viper.DecodeHook(mapstructure.ComposeDecodeHookFunc(
		viperutil.StringSliceViaEnvDecodeHook, viperutil.ByteSizeDecodeHook, viperutil.OrdererEndpointDecoder,
		durationDecoder, serverDecoder, endpointDecoder,
	))
}

func durationDecoder(dataType, targetType reflect.Type, rawData any) (result any, err error) {
	stringData, ok := viperutil.GetStringData(dataType, rawData)
	if !ok || targetType.Kind() != reflect.Int64 {
		return rawData, nil
	}
	duration, err := time.ParseDuration(stringData)
	return duration, errors.Wrap(err, "failed to parse duration")
}

func endpointDecoder(dataType, targetType reflect.Type, rawData any) (result any, err error) {
	stringData, ok := viperutil.GetStringData(dataType, rawData)
	if !ok || targetType != reflect.TypeFor[connection.Endpoint]() {
		return rawData, nil
	}
	endpoint, err := parseEndpoint(stringData)
	return endpoint, errors.Wrap(err, "failed to parse endpoint")
}

func serverDecoder(dataType, targetType reflect.Type, rawData any) (result any, err error) {
	stringData, ok := viperutil.GetStringData(dataType, rawData)
	if !ok || targetType != reflect.TypeFor[connection.ServerConfig]() {
		return rawData, nil
	}
	endpoint, err := parseEndpoint(stringData)
	var ret connection.ServerConfig
	if endpoint != nil {
		ret = connection.ServerConfig{Endpoint: *endpoint}
	}
	return ret, err
}

// parseEndpoint parses an endpoint from an address string.
func parseEndpoint(hostPort string) (*connection.Endpoint, error) {
	if len(hostPort) == 0 {
		return &connection.Endpoint{}, nil
	}
	hostName, portStr, err := net.SplitHostPort(hostPort)
	if err != nil {
		return nil, errors.Wrap(err, "could not split host and port")
	}
	err = validateHostName(hostName)
	if err != nil {
		return nil, err
	}
	portInt, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, errors.Wrap(err, "could not convert port to integer")
	}
	if portInt < 0 || portInt > maxPort {
		return nil, errors.Newf("port must be between 0 and %d, got %d", maxPort, portInt)
	}
	return &connection.Endpoint{
		Host: hostName,
		Port: portInt,
	}, nil
}

func validateHostName(hostName string) error {
	// An empty hostname is valid (e.g., :8080).
	if len(hostName) == 0 {
		return nil
	}
	// Check if it's an IPv6 address (IPv6 already extracted from brackets by SplitHostPort).
	if net.ParseIP(hostName) != nil {
		return nil
	}
	// Not an IP, validate as hostname.
	if len(hostName) > hostNameMaxLength {
		return errors.Newf("hostname exceeds maximum length of 253 characters")
	}
	// RFC 1123 hostname validation.
	if !hostNameRegex.MatchString(hostName) {
		return errors.Newf("invalid hostname: %s", hostName)
	}
	return nil
}
