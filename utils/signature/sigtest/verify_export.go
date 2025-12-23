/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package sigtest

import (
	"github.com/hyperledger/fabric-x-committer/api/applicationpb"
	"github.com/hyperledger/fabric-x-committer/utils/signature"
)

// NewNsVerifierFromKey instantiate a verifier given a public key and a scheme.
func NewNsVerifierFromKey(scheme string, key signature.PublicKey) (*signature.NsVerifier, error) {
	return signature.NewNsVerifier(&applicationpb.NamespacePolicy{
		Rule: &applicationpb.NamespacePolicy_ThresholdRule{
			ThresholdRule: &applicationpb.ThresholdRule{
				Scheme:    scheme,
				PublicKey: key,
			},
		},
	}, nil)
}
