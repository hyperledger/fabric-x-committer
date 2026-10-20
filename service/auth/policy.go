/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package auth

import (
	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-x-common/api/msppb"
	"github.com/hyperledger/fabric-x-common/common/channelconfig"
	"github.com/hyperledger/fabric-x-common/msp"
)

// policyReaders is the policy governing every resource the channel configuration's ACLs do not name.
const policyReaders = "/Channel/Application/Readers"

// ErrNoPolicyForResource is returned when the policy governing a resource is absent from the channel
// configuration.
var ErrNoPolicyForResource = errors.New("no policy defined for resource")

// evaluateResourcePolicy re-resolves the record's identity against the latest bundle, which is what makes
// a removed organization or rotated MSP take effect immediately, then evaluates the resource's policy.
func evaluateResourcePolicy(
	bundle *channelconfig.Bundle, resource string, clientIdentity *msppb.Identity,
) error {
	identity, err := bundle.MSPManager().DeserializeIdentity(clientIdentity)
	if err != nil {
		return errors.Wrap(err, "identity is no longer valid under the current configuration")
	}
	if err = identity.Validate(); err != nil {
		return errors.Wrap(err, "identity is not valid under the current configuration")
	}

	ref := resolvePolicyRef(bundle, resource)
	policy, ok := bundle.PolicyManager().GetPolicy(ref)
	if !ok {
		return errors.Wrapf(ErrNoPolicyForResource, "policy %s for resource %s is absent from the channel config",
			ref, resource)
	}
	if err = policy.EvaluateIdentities([]msp.Identity{identity}); err != nil {
		return errors.Wrap(err, "identity is not authorized by the resource policy")
	}
	return nil
}

// resolvePolicyRef returns the channel-policy reference governing a resource: the channel configuration's
// ACL for it, or the Application Readers policy when the configuration names none.
func resolvePolicyRef(bundle *channelconfig.Bundle, resource string) string {
	if app, ok := bundle.ApplicationConfig(); ok {
		if ref := app.APIPolicyMapper().PolicyRefForAPI(resource); ref != "" {
			return ref
		}
	}
	return policyReaders
}
