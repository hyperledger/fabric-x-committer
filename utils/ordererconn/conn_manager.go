/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package ordererconn

import (
	"fmt"
	"maps"
	"math"
	"math/rand"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/cockroachdb/errors"
	"google.golang.org/grpc"

	"github.com/hyperledger/fabric-x-committer/utils/connection"
)

type (
	// ConnectionManager packs all the orderer connections.
	// Its connections can be updated via the Update() method.
	// This will increase the config version, allowing a connection instance to identify
	// a connection update and fetch the new connections.
	ConnectionManager struct {
		configVersion atomic.Uint64
		connections   map[string]*grpc.ClientConn
		config        *ConnectionConfig
		lock          sync.Mutex
	}

	// ConnFilter is used to filter connections.
	ConnFilter struct {
		api string
		id  uint32
	}
)

// ErrNoConnections may be returned when trying to get the next connection.
var ErrNoConnections = errors.New("no connections found")

const (
	anyID     uint32 = math.MaxUint32
	anyAPI           = ""
	filterAll        = "all"
)

// WithAPI filters for API.
func WithAPI(api string) ConnFilter {
	return ConnFilter{api: api, id: anyID}
}

// WithID filters for ID.
func WithID(id uint32) ConnFilter {
	return ConnFilter{api: anyAPI, id: id}
}

func filterKey(filters ...ConnFilter) string {
	f := aggregateFilter(filters...)
	switch {
	case f.id == anyID && f.api != anyAPI:
		return filterAll
	case f.id == anyID:
		return fmt.Sprintf("api=%s", f.api)
	case f.api == anyAPI:
		return fmt.Sprintf("id=%d", f.id)
	default:
		return fmt.Sprintf("api=%s, id=%d", f.api, f.id)
	}
}

func aggregateFilter(filters ...ConnFilter) ConnFilter {
	k := ConnFilter{api: "", id: anyID}
	for _, f := range filters {
		if f.api != anyAPI {
			k.api = f.api
		}
		if f.id != anyID {
			k.id = f.id
		}
	}
	return k
}

func filterOrdererEndpoints(endpoints []*Endpoint, filters ...ConnFilter) []*connection.Endpoint {
	key := aggregateFilter(filters...)
	result := make([]*connection.Endpoint, 0, len(endpoints))
	for _, endpoint := range endpoints {
		if key.api != anyAPI && !endpoint.SupportsAPI(key.api) {
			continue
		}
		if key.id != anyID && endpoint.ID != key.id {
			continue
		}
		result = append(result, &endpoint.Endpoint)
	}
	return result
}

// Update updates the connection configs.
// This will close all connections, forcing the clients to reload.
func (c *ConnectionManager) Update(config *ConnectionConfig) error {
	if err := ValidateConnectionConfig(config); err != nil {
		return err
	}

	// We pre create all the connections to ensure correct form.
	connections := make(map[string]*grpc.ClientConn)
	allAPis := []string{anyAPI, Broadcast, Deliver}
	for _, id := range append(getAllIDs(config.Endpoints), anyID) {
		for _, api := range allAPis {
			filter := aggregateFilter(WithAPI(api), WithID(id))
			conn, err := openConnection(config, filter)
			if errors.Is(err, ErrNoConnections) {
				continue
			}
			if err != nil {
				closeConnection(connections)
				return err
			}
			connections[filterKey(filter)] = conn
		}
	}

	// We lock once we read internal members.
	c.lock.Lock()
	defer c.lock.Unlock()

	// We increase the version early (before closing any connections, but after locking)
	// to ensure the recovery stage knows about an update.
	c.configVersion.Add(1)
	closeConnection(c.connections)
	c.connections = connections
	c.config = config
	return nil
}

// GetConnection returns a connection given filters.
func (c *ConnectionManager) GetConnection(filters ...ConnFilter) (*grpc.ClientConn, uint64) {
	c.lock.Lock()
	defer c.lock.Unlock()
	v := c.configVersion.Load()
	if c.connections == nil {
		return nil, v
	}
	return c.connections[filterKey(filters...)], v
}

// GetConnectionPerID returns a connection given filters per ID.
func (c *ConnectionManager) GetConnectionPerID(filters ...ConnFilter) (map[uint32]*grpc.ClientConn, uint64) {
	c.lock.Lock()
	defer c.lock.Unlock()
	ret := make(map[uint32]*grpc.ClientConn)
	v := c.configVersion.Load()
	if c.connections == nil {
		return ret, v
	}
	filter := aggregateFilter(filters...)
	for _, id := range getAllIDs(c.config.Endpoints) {
		conn := c.connections[filterKey(filter, WithID(id))]
		if conn != nil {
			ret[id] = conn
		}
	}
	return ret, v
}

func getAllIDs(endpoints []*Endpoint) []uint32 {
	ids := make(map[uint32]any)
	for _, conn := range endpoints {
		ids[conn.ID] = nil
	}
	return slices.Collect(maps.Keys(ids))
}

func openConnection(
	conf *ConnectionConfig,
	filter ...ConnFilter,
) (*grpc.ClientConn, error) {
	key := aggregateFilter(filter...)

	endpoints := filterOrdererEndpoints(conf.Endpoints, key)
	if len(endpoints) == 0 {
		return nil, ErrNoConnections
	}
	// We shuffle the endpoints for load balancing.
	shuffle(endpoints)
	logger.Infof("Opening connections to %d endpoints: %v.", len(endpoints), endpoints)
	dialConfig, err := connection.NewLoadBalancedDialConfig(connection.MultiClientConfig{
		Endpoints: endpoints,
		Retry:     conf.Retry,
		TLS:       conf.TLS,
	})
	if err != nil {
		return nil, err
	}
	return connection.Connect(dialConfig)
}

// Close closes all the connections.
func (c *ConnectionManager) Close() {
	c.lock.Lock()
	defer c.lock.Unlock()
	closeConnection(c.connections)
	c.connections = nil
}

// IsStale checks if the given OrdererConnectionResiliencyManager is stale.
// If nil is given, it returns true.
func (c *ConnectionManager) IsStale(configVersion uint64) bool {
	return c.configVersion.Load() != configVersion
}

func closeConnection(connections map[string]*grpc.ClientConn) {
	if connections != nil {
		connection.CloseConnectionsLog(slices.Collect(maps.Values(connections))...)
	}
}

func shuffle[T any](nodes []T) {
	rand.Shuffle(len(nodes), func(i, j int) { nodes[i], nodes[j] = nodes[j], nodes[i] })
}
