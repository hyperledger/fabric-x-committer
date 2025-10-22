/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package config

import (
	"reflect"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/spf13/viper"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/ordererconn"
)

type decoderFunc = func(dataType, targetType reflect.Type, rawData any) (any, bool, error)

var decoders = []decoderFunc{durationDecoder, serverDecoder, endpointDecoder, ordererEndpointDecoder}

// decoderHook contains custom unmarshalling for types not supported by default by mapstructure.
// I.e., [time.Duration], [connection.Endpoint], [ordererconn.Endpoint].
func decoderHook(hooks ...decoderFunc) viper.DecoderConfigOption {
	return viper.DecodeHook(func(dataType, targetType reflect.Type, rawData any) (any, error) {
		for _, hook := range hooks {
			if result, done, err := hook(dataType, targetType, rawData); done {
				return result, err
			}
		}
		return rawData, nil
	})
}

func durationDecoder(dataType, targetType reflect.Type, rawData any) (result any, done bool, err error) {
	stringData, ok := getStringData(dataType, rawData)
	if !ok || targetType.Kind() != reflect.Int64 {
		return nil, false, nil
	}
	duration, err := time.ParseDuration(stringData)
	return duration, true, errors.Wrap(err, "failed to parse duration")
}

func endpointDecoder(dataType, targetType reflect.Type, rawData any) (result any, done bool, err error) {
	stringData, ok := getStringData(dataType, rawData)
	if !ok || targetType != reflect.TypeOf(connection.Endpoint{}) {
		return nil, false, nil
	}
	endpoint, err := connection.NewEndpoint(stringData)
	return endpoint, true, errors.Wrap(err, "failed to parse endpoint")
}

func ordererEndpointDecoder(dataType, targetType reflect.Type, rawData any) (result any, done bool, err error) {
	stringData, ok := getStringData(dataType, rawData)
	if !ok || targetType != reflect.TypeOf(ordererconn.Endpoint{}) {
		return nil, false, nil
	}
	endpoint, err := ordererconn.ParseEndpoint(stringData)
	return endpoint, true, errors.Wrap(err, "failed to parse orderer endpoint")
}

func serverDecoder(dataType, targetType reflect.Type, rawData any) (result any, done bool, err error) {
	stringData, ok := getStringData(dataType, rawData)
	if !ok || targetType != reflect.TypeOf(connection.ServerConfig{}) {
		return nil, false, nil
	}
	endpoint, err := connection.NewEndpoint(stringData)
	var ret connection.ServerConfig
	if endpoint != nil {
		ret = connection.ServerConfig{Endpoint: *endpoint}
	}
	return ret, true, err
}

func getStringData(dataType reflect.Type, rawData any) (stringData string, isStringData bool) {
	if dataType.Kind() != reflect.String {
		return stringData, false
	}
	stringData, isStringData = rawData.(string)
	return stringData, isStringData
}
