/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package test

import "sync/atomic"

// MockTLSUpdater is a thread-safe mock for connection.TLSCertUpdater,
// used by tests that verify dynamic TLS CA rotation.
type MockTLSUpdater struct {
	certs atomic.Pointer[[][]byte]
}

// SetClientRootCAs stores the given certs.
func (m *MockTLSUpdater) SetClientRootCAs(certs [][]byte) error {
	m.certs.Store(&certs)
	return nil
}

// LastCerts returns the most recently stored certs.
func (m *MockTLSUpdater) LastCerts() [][]byte {
	if p := m.certs.Load(); p != nil {
		return *p
	}
	return nil
}
