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
	"strings"
	"sync"
	"sync/atomic"

	"github.com/cockroachdb/errors"
	commontypes "github.com/hyperledger/fabric-x-common/api/types"
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
		endpoints     []*commontypes.OrdererEndpoint
		lock          sync.Mutex
		retry         *connection.RetryProfile
		tlsParameters *connection.TLSMaterials
	}

	// ConnFilter is used to filter connections.
	ConnFilter struct {
		api string
		id  uint32
	}

	// openConnectionParameters is the orderer client config with tls parameters already loaded bytes.
	openConnectionParameters struct {
		Endpoints []*connection.Endpoint
		TLS       *connection.TLSMaterials
		Retry     *connection.RetryProfile
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

// NewConnectionManager constructs a ConnectionManager and initializes its connections.
// If orgMaterial is provided, it updates the connection manager accordingly.
// Otherwise, it uses the organization configuration.
func NewConnectionManager(config *Config) (*ConnectionManager, error) {
	tlsParams, err := connection.NewTLSMaterials(connection.TLSConfig{
		Mode:        config.TLS.Mode,
		CertPath:    config.TLS.CertPath,
		KeyPath:     config.TLS.KeyPath,
		CACertPaths: config.TLS.CommonCACertPaths,
	})
	if err != nil {
		return nil, err
	}
	// create connection manager with the config's retry policy and TLS.
	cm := &ConnectionManager{
		tlsParameters: tlsParams,
		retry:         config.Retry,
	}
	orgsMaterial, err := config.OrganizationsConfigToMaterials()
	if err != nil {
		return nil, err
	}
	if err = cm.Update(orgsMaterial); err != nil {
		return nil, err
	}
	return cm, nil
}

// Update updates the orderer connections.
// This will close all connections, forcing the clients to reload.
// Complexity is inherent: this function atomically builds and cache connections across organizations.
func (cm *ConnectionManager) Update(orgsMat []*OrganizationMaterial) error { //nolint:gocognit
	if err := ValidateOrganizations(orgsMat...); err != nil {
		return err
	}
	// We pre create all the connections to ensure correct form.
	connections := make(map[string]*grpc.ClientConn)
	// We use a connection cache to avoid opening the same connection multiple times.
	connCache := make(map[string]*grpc.ClientConn)
	allAPIs := []string{anyAPI, Broadcast, Deliver}
	// We save the endpoints for later processing.
	var allOrgsEndpoints []*commontypes.OrdererEndpoint
	for _, org := range orgsMat {
		orgTLSParams := *cm.tlsParameters
		orgTLSParams.CACerts = append(orgTLSParams.CACerts, org.CACerts...)
		for _, id := range append(getAllIDs(org.Endpoints), anyID) {
			for _, api := range allAPIs {
				filter := aggregateFilter(WithAPI(api), WithID(id))
				endpoints := filterOrdererEndpoints(org.Endpoints, filter)
				if len(endpoints) == 0 {
					continue
				}
				endpointsKey := makeEndpointsKey(endpoints)
				conn, connInCache := connCache[endpointsKey]
				if !connInCache {
					var err error
					conn, err = openConnection(&openConnectionParameters{
						Endpoints: endpoints,
						TLS:       &orgTLSParams,
						Retry:     cm.retry,
					})
					if err != nil {
						closeConnection(connections)
						return err
					}
					connCache[endpointsKey] = conn
				}
				connections[filterKey(filter)] = conn
			}
		}
		allOrgsEndpoints = append(allOrgsEndpoints, org.Endpoints...)
	}

	// We lock once we read internal members.
	cm.lock.Lock()
	defer cm.lock.Unlock()

	// We increase the version early (before closing any connections, but after locking)
	// to ensure the recovery stage knows about an update.
	cm.configVersion.Add(1)
	closeConnection(cm.connections)
	cm.connections = connections
	cm.endpoints = allOrgsEndpoints
	return nil
}

// GetConnection returns a connection given filters.
func (cm *ConnectionManager) GetConnection(filters ...ConnFilter) (*grpc.ClientConn, uint64) {
	cm.lock.Lock()
	defer cm.lock.Unlock()
	v := cm.configVersion.Load()
	if cm.connections == nil {
		return nil, v
	}
	return cm.connections[filterKey(filters...)], v
}

// GetConnectionPerID returns a connection given filters per ID.
func (cm *ConnectionManager) GetConnectionPerID(filters ...ConnFilter) (map[uint32]*grpc.ClientConn, uint64) {
	cm.lock.Lock()
	defer cm.lock.Unlock()
	ret := make(map[uint32]*grpc.ClientConn)
	v := cm.configVersion.Load()
	if cm.connections == nil {
		return ret, v
	}
	filter := aggregateFilter(filters...)
	for _, id := range getAllIDs(cm.endpoints) {
		conn := cm.connections[filterKey(filter, WithID(id))]
		if conn != nil {
			ret[id] = conn
		}
	}
	return ret, v
}

func getAllIDs(endpoints []*commontypes.OrdererEndpoint) []uint32 {
	ids := make(map[uint32]any)
	for _, conn := range endpoints {
		ids[conn.ID] = nil
	}
	return slices.Collect(maps.Keys(ids))
}

func openConnection(
	params *openConnectionParameters,
) (*grpc.ClientConn, error) {
	// We shuffle the endpoints for load balancing.
	shuffle(params.Endpoints)
	logger.Infof("Opening connections to %d endpoints: %v.", len(params.Endpoints), params.Endpoints)
	return connection.NewLoadBalancedConnectionForOrderer(params.Endpoints, params.TLS, params.Retry)
}

func makeEndpointsKey(endpoint []*connection.Endpoint) string {
	addresses := make([]string, len(endpoint))
	for i, e := range endpoint {
		addresses[i] = e.Address()
	}
	slices.Sort(addresses)
	return strings.Join(addresses, ";")
}

// CloseConnections closes all the connections.
func (cm *ConnectionManager) CloseConnections() {
	cm.lock.Lock()
	defer cm.lock.Unlock()
	closeConnection(cm.connections)
	cm.connections = nil
}

// IsStale checks if the given OrdererConnectionResiliencyManager is stale.
// If nil is given, it returns true.
func (cm *ConnectionManager) IsStale(configVersion uint64) bool {
	return cm.configVersion.Load() != configVersion
}

func closeConnection(connections map[string]*grpc.ClientConn) {
	if connections != nil {
		connection.CloseConnectionsLog(slices.Collect(maps.Values(connections))...)
	}
}

func shuffle[T any](nodes []T) {
	rand.Shuffle(len(nodes), func(i, j int) { nodes[i], nodes[j] = nodes[j], nodes[i] })
}

func filterOrdererEndpoints(endpoints []*commontypes.OrdererEndpoint, filters ...ConnFilter) []*connection.Endpoint {
	key := aggregateFilter(filters...)
	result := make([]*connection.Endpoint, 0, len(endpoints))
	for _, ep := range endpoints {
		if key.api != anyAPI && !ep.SupportsAPI(key.api) {
			continue
		}
		if key.id != anyID && ep.ID != key.id {
			continue
		}
		result = append(result, &connection.Endpoint{Host: ep.Host, Port: ep.Port})
	}
	return result
}
