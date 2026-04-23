/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

// LogStruct logs a struct in a flat representation.
func LogStruct(t *testing.T, name string, v any) {
	t.Helper()
	// Marshal struct to JSON.
	data, err := json.Marshal(v)
	require.NoError(t, err)

	// Unmarshal to map.
	var m map[string]any
	err = json.Unmarshal(data, &m)
	require.NoError(t, err)

	// Flatten.
	flat := make(map[string]any)
	flatten("", m, flat)
	sb := &strings.Builder{}
	for _, k := range slices.Sorted(maps.Keys(flat)) {
		sb.WriteString(k)
		sb.WriteString(": ")
		_, printErr := fmt.Fprintf(sb, "%v", flat[k])
		require.NoError(t, printErr)
		sb.WriteString("\n")
	}
	t.Logf("%s:\n%s", name, sb.String())
}

func flatten(prefix string, in any, out map[string]any) {
	switch val := in.(type) {
	case map[string]any:
		e := flattenEndpoint(val)
		if e != nil {
			out[prefix] = e
			return
		}
		for k, v := range val {
			key := k
			if prefix != "" {
				key = prefix + "." + k
			}
			flatten(key, v, out)
		}
	case []any:
		for i, item := range val {
			key := fmt.Sprintf("%s.%d", prefix, i)
			flatten(key, item, out)
		}
	default:
		out[prefix] = val
	}
}

func flattenEndpoint(in map[string]any) *connection.Endpoint {
	if len(in) != 2 {
		return nil
	}
	host, okHost := in["host"]
	if !okHost {
		return nil
	}
	hostStr, okHostStr := host.(string)
	if !okHostStr {
		return nil
	}
	port, okPort := in["port"]
	if !okPort {
		return nil
	}
	portFloat, okPortFloat := port.(float64)
	if !okPortFloat {
		return nil
	}
	return &connection.Endpoint{Host: hostStr, Port: int(portFloat)}
}
