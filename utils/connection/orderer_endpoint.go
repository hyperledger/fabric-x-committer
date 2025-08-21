/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package connection

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"

	"github.com/cockroachdb/errors"
	"gopkg.in/yaml.v3"
)

type (
	// OrdererEndpoint defines a party's endpoint.
	OrdererEndpoint struct {
		// ID is the concenter's ID (party).
		ID    uint32 `mapstructure:"id" json:"id,omitempty" yaml:"id,omitempty"`
		MspID string `mapstructure:"msp-id" json:"msp-id,omitempty" yaml:"msp-id,omitempty"`
		// API should be broadcast and/or deliver.
		API      []string `mapstructure:"api" json:"api,omitempty" yaml:"api,omitempty"`
		Endpoint `mapstructure:",squash" yaml:",inline"`
	}
)

const (
	// Broadcast support by endpoint.
	Broadcast = "broadcast"
	// Deliver support by endpoint.
	Deliver = "deliver"
)

// Orderer endpoints errors.
var (
	ErrInvalidEndpointKey = errors.New("invalid endpoint key")
	ErrInvalidEndpoint    = errors.New("invalid endpoint")
)

// NewOrdererEndpoints is a helper function to generate a list of OrdererEndpoint(s) from ServerConfig(s).
func NewOrdererEndpoints(id uint32, msp string, configs ...*ServerConfig) []*OrdererEndpoint {
	ordererEndpoints := make([]*OrdererEndpoint, len(configs))
	for i, c := range configs {
		ordererEndpoints[i] = &OrdererEndpoint{
			ID:       id,
			MspID:    msp,
			API:      []string{Broadcast, Deliver},
			Endpoint: c.Endpoint,
		}
	}
	return ordererEndpoints
}

// String returns a deterministic representation of the endpoint.
func (e *OrdererEndpoint) String() string {
	var output strings.Builder
	output.WriteString("id=")
	output.WriteString(strconv.FormatUint(uint64(e.ID), 10))
	if len(e.MspID) > 0 {
		output.WriteString(",msp-id=")
		output.WriteString(e.MspID)
	}
	for _, api := range e.API {
		output.WriteRune(',')
		output.WriteString(api)
	}
	if len(e.Host) > 0 || e.Port > 0 {
		output.WriteRune(',')
		output.WriteString(e.Host)
		output.WriteRune(':')
		output.WriteString(strconv.FormatInt(int64(e.Port), 10))
	}
	return output.String()
}

// SupportsAPI returns true if this endpoint supports API.
// It also returns true if no APIs are specified, as we cannot know.
func (e *OrdererEndpoint) SupportsAPI(api string) bool {
	return len(e.API) == 0 || slices.Contains(e.API, api)
}

// ParseOrdererEndpoint parses a string according to the following schema order (the first that succeeds).
// Schema 1: JSON.
// Schema 2: YAML.
// Schema 3: [id=ID,][msp-id=MspID,][broadcast,][deliver,][host=Host,][port=Port,][Host:Port].
func ParseOrdererEndpoint(valueRaw string) (*OrdererEndpoint, error) {
	ret := &OrdererEndpoint{}
	if len(valueRaw) == 0 {
		return ret, nil
	}
	if err := json.Unmarshal([]byte(valueRaw), ret); err == nil {
		return ret, nil
	}
	if err := yaml.Unmarshal([]byte(valueRaw), ret); err == nil {
		return ret, nil
	}
	err := unmarshalOrdererEndpoint(valueRaw, ret)
	return ret, err
}

func unmarshalOrdererEndpoint(valueRaw string, out *OrdererEndpoint) error {
	metaParts := strings.Split(valueRaw, ",")
	for _, item := range metaParts {
		item = strings.TrimSpace(item)
		equalIdx := strings.Index(item, "=")
		colonIdx := strings.Index(item, ":")
		var err error
		switch {
		case item == Broadcast || item == Deliver:
			out.API = append(out.API, item)
		case equalIdx >= 0:
			key, value := strings.TrimSpace(item[:equalIdx]), strings.TrimSpace(item[equalIdx+1:])
			switch key {
			case "msp-id":
				out.MspID = value
			case "host":
				out.Host = value
			case "id":
				err = out.setID(value)
			case "port":
				err = out.setPort(value)
			default:
				return ErrInvalidEndpointKey
			}
		case colonIdx >= 0:
			out.Host = strings.TrimSpace(item[:colonIdx])
			err = out.setPort(strings.TrimSpace(item[colonIdx+1:]))
		default:
			return ErrInvalidEndpoint
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (e *OrdererEndpoint) setPort(portStr string) error {
	port, err := strconv.ParseInt(portStr, 10, 32)
	if err != nil {
		return errors.Wrap(err, "failed to parse port")
	}
	e.Port = int(port)
	return nil
}

func (e *OrdererEndpoint) setID(idStr string) error {
	id, err := strconv.ParseUint(idStr, 10, 32)
	if err != nil {
		return errors.Wrap(err, "invalid id value")
	}
	e.ID = uint32(id)
	return nil
}
