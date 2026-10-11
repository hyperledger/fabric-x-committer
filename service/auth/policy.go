/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package auth

import (
	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-protos-go-apiv2/peer"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/hyperledger/fabric-x-common/api/msppb"
	"github.com/hyperledger/fabric-x-common/common/channelconfig"
	"github.com/hyperledger/fabric-x-common/msp"
)

// policyReaders is the Application Readers policy every exposed resource maps to by default.
const policyReaders = "/Channel/Application/Readers"

var (
	// ErrNoPolicyForResource is returned when neither the channel configuration's ACLs section nor the
	// built-in default map defines a policy for a resource.
	ErrNoPolicyForResource = errors.New("no policy defined for resource")

	// defaultResourcePolicy is the fallback consulted when the channel configuration defines no ACL for a
	// resource. Every exposed method defaults to the Application Readers policy.
	defaultResourcePolicy = map[string]string{
		committerpb.QueryService_GetRows_FullMethodName:              policyReaders,
		committerpb.QueryService_BeginView_FullMethodName:            policyReaders,
		committerpb.QueryService_EndView_FullMethodName:              policyReaders,
		committerpb.QueryService_GetNamespacePolicies_FullMethodName: policyReaders,
		committerpb.QueryService_GetConfigTransaction_FullMethodName: policyReaders,
		committerpb.QueryService_GetTransactionStatus_FullMethodName: policyReaders,

		committerpb.SidecarService_GetBlockchainInfo_FullMethodName: policyReaders,
		committerpb.SidecarService_GetBlockByNumber_FullMethodName:  policyReaders,
		committerpb.SidecarService_GetBlockByTxID_FullMethodName:    policyReaders,
		committerpb.SidecarService_GetTxByID_FullMethodName:         policyReaders,

		committerpb.SidecarService_OpenNotificationStream_FullMethodName: policyReaders,
		committerpb.SidecarService_StreamBlocks_FullMethodName:           policyReaders,

		peer.Deliver_Deliver_FullMethodName:                policyReaders,
		peer.Deliver_DeliverFiltered_FullMethodName:        policyReaders,
		peer.Deliver_DeliverWithPrivateData_FullMethodName: policyReaders,
	}
)

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
	if ref == "" {
		return errors.Wrapf(ErrNoPolicyForResource, "resource %s", resource)
	}
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

// resolvePolicyRef returns the channel-policy reference governing a resource, preferring the channel
// configuration's ACLs over the built-in defaults. It returns "" when neither defines the resource.
func resolvePolicyRef(bundle *channelconfig.Bundle, resource string) string {
	if app, ok := bundle.ApplicationConfig(); ok {
		if ref := app.APIPolicyMapper().PolicyRefForAPI(resource); ref != "" {
			return ref
		}
	}
	return defaultResourcePolicy[resource]
}
