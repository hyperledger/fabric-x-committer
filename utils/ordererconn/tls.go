/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package ordererconn

import "github.com/hyperledger/fabric-x-committer/utils/connection"

// NewTLSMaterials is a wrapper for [connection.NewTLSMaterials] with the orderer's config.
func NewTLSMaterials(c OrdererTLSConfig) (*connection.TLSMaterials, error) {
	return connection.NewTLSMaterials(connection.TLSConfig{
		Mode:        c.Mode,
		KeyPath:     c.KeyPath,
		CertPath:    c.CertPath,
		CACertPaths: c.CommonCACertPaths,
	})
}
