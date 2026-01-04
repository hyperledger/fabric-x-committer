/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package config

import (
	"reflect"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-x-common/common/viperutil"
	"github.com/mitchellh/mapstructure"
	"github.com/spf13/viper"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
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
	if !ok || targetType != reflect.TypeOf(connection.Endpoint{}) {
		return rawData, nil
	}
	endpoint, err := connection.NewEndpoint(stringData)
	return endpoint, errors.Wrap(err, "failed to parse endpoint")
}

func serverDecoder(dataType, targetType reflect.Type, rawData any) (result any, err error) {
	stringData, ok := viperutil.GetStringData(dataType, rawData)
	if !ok || targetType != reflect.TypeOf(connection.ServerConfig{}) {
		return rawData, nil
	}
	endpoint, err := connection.NewEndpoint(stringData)
	var ret connection.ServerConfig
	if endpoint != nil {
		ret = connection.ServerConfig{Endpoint: *endpoint}
	}
	return ret, err
}
