/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package config

import (
	"bytes"
	"testing"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/ordererconn"
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
		Server                       connection.ServerConfig `mapstructure:"server"`
		Endpoint                     connection.Endpoint     `mapstructure:"endpoint"`
		OrdererEndpoint              ordererconn.Endpoint    `mapstructure:"orderer-endpoint"`
		JSONOrdererEndpoint          ordererconn.Endpoint    `mapstructure:"json-orderer-endpoint"`
		MultilineJSONOrdererEndpoint ordererconn.Endpoint    `mapstructure:"multiline-json-orderer-endpoint"`
		YamlJSONOrdererEndpoint      ordererconn.Endpoint    `mapstructure:"yaml-orderer-endpoint"`
	})
	require.NoError(t, unmarshal(v, conf))
	expected := ordererconn.Endpoint{
		ID:    5,
		MspID: "org",
		API:   []string{"broadcast", "deliver"},
		Endpoint: connection.Endpoint{
			Host: "localhost",
			Port: 5050,
		},
	}
	require.Equal(t, expected.Endpoint, conf.Server.Endpoint)
	require.Equal(t, expected.Endpoint, conf.Endpoint)
	require.Equal(t, expected, conf.OrdererEndpoint)
	require.Equal(t, expected, conf.JSONOrdererEndpoint)
	require.Equal(t, expected, conf.MultilineJSONOrdererEndpoint)
	require.Equal(t, expected, conf.YamlJSONOrdererEndpoint)
}
