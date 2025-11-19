/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package config

import (
	"bytes"
	"testing"

	commontypes "github.com/hyperledger/fabric-x-common/api/types"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

const config = `
server: localhost:5050
endpoint: localhost:5050
orderer-endpoint: id=5,msp-id=org,broadcast,deliver,localhost:5050
json-orderer-endpoint: {"id":5,"msp-id":"org","api":["broadcast","deliver"],"host":"localhost","port":5050}
multiline-json-orderer-endpoint: >
    {
        "id": 5,
        "msp-id": "org",
        "api": ["broadcast","deliver"],
        "host": "localhost",
        "port": 5050
    }
yaml-orderer-endpoint:
    id: 5
    msp-id: org
    api:
        - broadcast
        - deliver
    host: localhost
    port: 5050
`

func TestEndpoints(t *testing.T) {
	t.Parallel()
	v := viper.New()
	require.NoError(t, readYamlConfigsFromIO(v, bytes.NewBufferString(config)))
	conf := new(struct {
		Server                       connection.ServerConfig     `mapstructure:"server"`
		Endpoint                     connection.Endpoint         `mapstructure:"endpoint"`
		OrdererEndpoint              commontypes.OrdererEndpoint `mapstructure:"orderer-endpoint"`
		JSONOrdererEndpoint          commontypes.OrdererEndpoint `mapstructure:"json-orderer-endpoint"`
		MultilineJSONOrdererEndpoint commontypes.OrdererEndpoint `mapstructure:"multiline-json-orderer-endpoint"`
		YamlJSONOrdererEndpoint      commontypes.OrdererEndpoint `mapstructure:"yaml-orderer-endpoint"`
	})
	require.NoError(t, unmarshal(v, conf))
	expected := commontypes.OrdererEndpoint{
		ID:    5,
		MspID: "org",
		API:   []string{"broadcast", "deliver"},
		Host:  "localhost",
		Port:  5050,
	}
	expectedEndpoint := connection.Endpoint{
		Host: "localhost",
		Port: 5050,
	}
	require.Equal(t, expectedEndpoint, conf.Server.Endpoint)
	require.Equal(t, expectedEndpoint, conf.Endpoint)
	require.Equal(t, expected, conf.OrdererEndpoint)
	require.Equal(t, expected, conf.JSONOrdererEndpoint)
	require.Equal(t, expected, conf.MultilineJSONOrdererEndpoint)
	require.Equal(t, expected, conf.YamlJSONOrdererEndpoint)
}
