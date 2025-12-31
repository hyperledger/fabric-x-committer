/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package config

import (
	"reflect"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
	commontypes "github.com/hyperledger/fabric-x-common/api/types"
	"github.com/hyperledger/fabric-x-common/common/viperutil"
	"github.com/mitchellh/mapstructure"
	"github.com/spf13/viper"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/ordererconn"
)

// decoderHook contains custom unmarshalling for types not supported by default by mapstructure.
func decoderHook() viper.DecoderConfigOption {
	return viper.DecodeHook(mapstructure.ComposeDecodeHookFunc(
		viperutil.StringSliceViaEnvDecodeHook, viperutil.ByteSizeDecodeHook, viperutil.OrdererEndpointDecoder,
		durationDecoder, serverDecoder, endpointDecoder, organizationDecoder,
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

func organizationDecoder(dataType, targetType reflect.Type, rawData any) (any, error) {
	stringData, ok := viperutil.GetStringData(dataType, rawData)
	if !ok || targetType != reflect.TypeOf(ordererconn.OrganizationConfig{}) {
		return rawData, nil
	}
	return parseOrganizationConfig(stringData)
}

func parseOrganizationConfig(valueRaw string) (ordererconn.OrganizationConfig, error) {
	out := ordererconn.OrganizationConfig{}
	for _, item := range strings.Split(valueRaw, ";") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		switch {
		case strings.HasPrefix(item, "msp-id="):
			out.MspID = strings.TrimPrefix(item, "msp-id=")
		case strings.HasPrefix(item, "ca="):
			caPaths := strings.TrimPrefix(item, "ca=")
			out.CACerts = append(out.CACerts, strings.Split(caPaths, ",")...)
		default:
			// If item != (msp-id and ca), we assume it's orderer endpoints string
			// and delegate to the endpoint parser
			ep, err := commontypes.ParseOrdererEndpoint(item)
			if err != nil {
				return out, err
			}
			out.Endpoints = append(out.Endpoints, ep)
		}
	}
	return out, nil
}
