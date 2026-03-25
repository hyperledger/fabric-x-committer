/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package ordererdial

import "github.com/hyperledger/fabric-x-committer/utils/connection"

// TLSConfigToOrdererTLSConfig translates a connection.TLSConfig to an TLSConfig.
func TLSConfigToOrdererTLSConfig(c connection.TLSConfig) TLSConfig {
	return TLSConfig{
		Mode:              c.Mode,
		KeyPath:           c.KeyPath,
		CertPath:          c.CertPath,
		CommonCACertPaths: c.CACertPaths,
	}
}
