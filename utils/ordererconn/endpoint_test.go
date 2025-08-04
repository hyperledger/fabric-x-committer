/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package ordererconn

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

func TestReadWrite(t *testing.T) {
	t.Parallel()
	valSchema := "id=5,msp-id=org,broadcast,deliver,localhost:5050"
	valJSON := `{"id":5,"msp-id":"org","api":["broadcast","deliver"],"host":"localhost","port":5050}`
	valYAML := `id: 5
msp-id: org
api:
    - broadcast
    - deliver
host: localhost
port: 5050
`
	expected := &Endpoint{
		ID:    5,
		MspID: "org",
		API:   []string{Broadcast, Deliver},
		Endpoint: connection.Endpoint{
			Host: "localhost",
			Port: 5050,
		},
	}
	require.Equal(t, valSchema, expected.String())

	valJSONRaw, err := json.Marshal(expected)
	require.NoError(t, err)
	require.JSONEq(t, valJSON, string(valJSONRaw))

	valYamlRaw, err := yaml.Marshal(expected)
	require.NoError(t, err)
	require.YAMLEq(t, valYAML, string(valYamlRaw))

	e, err := ParseEndpoint(valSchema)
	require.NoError(t, err)
	require.Equal(t, expected, e)

	e, err = ParseEndpoint(valJSON)
	require.NoError(t, err)
	require.Equal(t, expected, e)

	e, err = ParseEndpoint(valYAML)
	require.NoError(t, err)
	require.Equal(t, expected, e)
}
