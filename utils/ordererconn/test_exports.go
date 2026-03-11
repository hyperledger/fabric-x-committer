/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package ordererconn

import "github.com/hyperledger/fabric-x-committer/utils/connection"

// TLSConfigToOrdererTLSConfig translates a connection.TLSConfig to an OrdererTLSConfig.
func TLSConfigToOrdererTLSConfig(c connection.TLSConfig) OrdererTLSConfig {
	return OrdererTLSConfig{
		Mode:              c.Mode,
		KeyPath:           c.KeyPath,
		CertPath:          c.CertPath,
		CommonCACertPaths: c.CACertPaths,
	}
}
