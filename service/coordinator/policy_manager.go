/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package coordinator

import (
	"maps"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
	"github.com/hyperledger/fabric-x-committer/api/protosigverifierservice"
	"github.com/hyperledger/fabric-x-committer/service/verifier/policy"
)

// policyManager is responsible for locally holding the latest namespace policies and config transaction
// according to the processed TXs. It does not parse these policies.
type policyManager struct {
	latestVersion     atomic.Uint64
	nsPolicies        map[string]*protoblocktx.PolicyItem
	nsVersions        map[string]uint64
	configTransaction *protoblocktx.ConfigTransaction
	configVersion     uint64
	lock              sync.Mutex
}

func newPolicyManager() *policyManager {
	return &policyManager{
		nsPolicies: make(map[string]*protoblocktx.PolicyItem),
		nsVersions: make(map[string]uint64),
	}
}

func (pm *policyManager) updateFromTx(namespaces []*protoblocktx.TxNamespace) {
	var updates []*protosigverifierservice.Update
	for _, ns := range namespaces {
		if u := policy.GetUpdatesFromNamespace(ns); u != nil {
			updates = append(updates, u)
		}
	}
	pm.update(updates...)
}

func (pm *policyManager) update(update ...*protosigverifierservice.Update) {
	// Prevent unnecessary version increments.
	if isUpdateEmpty(update...) {
		return
	}

	pm.lock.Lock()
	defer pm.lock.Unlock()
	version := pm.latestVersion.Add(1)
	for _, u := range update {
		if u == nil {
			continue
		}
		if u.Config != nil {
			pm.configTransaction = u.Config
			pm.configVersion = version
		}
		if u.NamespacePolicies != nil {
			for _, p := range u.NamespacePolicies.Policies {
				pm.nsPolicies[p.Namespace] = p
				pm.nsVersions[p.Namespace] = version
			}
		}
	}
}

func isUpdateEmpty(update ...*protosigverifierservice.Update) bool {
	for _, u := range update {
		if u != nil && (u.Config != nil || u.NamespacePolicies != nil) {
			return false
		}
	}
	return true
}

func (pm *policyManager) getAll() (*protosigverifierservice.Update, uint64) {
	pm.lock.Lock()
	defer pm.lock.Unlock()
	return &protosigverifierservice.Update{
		NamespacePolicies: &protoblocktx.NamespacePolicies{
			Policies: slices.Collect(maps.Values(pm.nsPolicies)),
		},
		Config: pm.configTransaction,
	}, pm.latestVersion.Load()
}

func (pm *policyManager) getUpdates(version uint64) (*protosigverifierservice.Update, uint64) {
	if version == pm.latestVersion.Load() {
		return nil, version
	}

	pm.lock.Lock()
	defer pm.lock.Unlock()
	ret := &protosigverifierservice.Update{}
	if version < pm.configVersion {
		ret.Config = pm.configTransaction
	}

	nsUpdates := make([]*protoblocktx.PolicyItem, 0, len(pm.nsPolicies))
	for ns, v := range pm.nsVersions {
		if version < v {
			nsUpdates = append(nsUpdates, pm.nsPolicies[ns])
		}
	}
	if len(nsUpdates) > 0 {
		ret.NamespacePolicies = &protoblocktx.NamespacePolicies{Policies: nsUpdates}
	}

	return ret, pm.latestVersion.Load()
}
