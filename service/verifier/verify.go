/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package verifier

import (
	"maps"
	"slices"
	"sync/atomic"

	"github.com/cockroachdb/errors"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
	"github.com/hyperledger/fabric-x-committer/api/protosigverifierservice"
	"github.com/hyperledger/fabric-x-committer/api/types"
	"github.com/hyperledger/fabric-x-committer/service/verifier/policy"
	"github.com/hyperledger/fabric-x-committer/utils"
	"github.com/hyperledger/fabric-x-committer/utils/signature"
)

type verifier struct {
	verifiers atomic.Pointer[map[string]*signature.NsVerifier]
}

func newVerifier() *verifier {
	logger.Info("Initializing new verifier")
	v := &verifier{}
	verifiers := make(map[string]*signature.NsVerifier)
	v.verifiers.Store(&verifiers)
	return v
}

// updatePolicies updates the verifier's policies.
// We assume no parallel update calls, thus, no lock is required.
func (v *verifier) updatePolicies(
	update *protosigverifierservice.Update,
) error {
	if update == nil || (update.Config == nil && update.NamespacePolicies == nil) {
		return nil
	}
	// We parse the policy during validation and mark transactions as invalid if parsing fails.
	// While it is unlikely that policy parsing would fail at this stage, it could happen
	// if the stored policy in the database is corrupted or maliciously altered, or if there is a
	// bug in the committer that modifies the policy bytes.
	newVerifiers, err := parsePolicies(update)
	if err != nil {
		return errors.Join(ErrUpdatePolicies, err)
	}

	defer logger.Infof("New verification policies for namespaces %v", slices.Collect(maps.Keys(newVerifiers)))

	for k, nsVerifier := range *v.verifiers.Load() {
		if _, ok := newVerifiers[k]; !ok {
			newVerifiers[k] = nsVerifier
		}
	}
	v.verifiers.Store(&newVerifiers)
	return nil
}

func parsePolicies(update *protosigverifierservice.Update) (map[string]*signature.NsVerifier, error) {
	newPolicies := make(map[string]*signature.NsVerifier)
	if update.Config != nil {
		nsVerifier, err := policy.ParsePolicyFromConfigTx(update.Config.Envelope)
		if err != nil {
			return nil, errors.Join(ErrUpdatePolicies, err)
		}
		newPolicies[types.MetaNamespaceID] = nsVerifier
	}
	if update.NamespacePolicies != nil {
		for _, pd := range update.NamespacePolicies.Policies {
			nsVerifier, err := policy.ParseNamespacePolicyItem(pd)
			if err != nil {
				return nil, errors.Join(ErrUpdatePolicies, err)
			}
			newPolicies[pd.Namespace] = nsVerifier
		}
	}
	return newPolicies, nil
}

func (v *verifier) verifyRequest(request *protosigverifierservice.Request) *protosigverifierservice.Response {
	logger.Debugf("Validating TX [%d:%d]: %s", request.BlockNum, request.TxNum, &utils.LazyJSON{O: request.Tx})
	response := &protosigverifierservice.Response{
		TxId:     request.Tx.Id,
		BlockNum: request.BlockNum,
		TxNum:    request.TxNum,
		Status:   protoblocktx.Status_COMMITTED,
	}
	// The verifiers might temporarily retain the old map while updatePolicies has already set a new one.
	// This is acceptable, provided the coordinator sends the validation status to the dependency graph
	// after updating the policies in the verifier.
	// This ensures that dependent data transactions on these updated namespaces always use the map
	// containing the latest policy.
	verifiers := *v.verifiers.Load()
	for nsIndex, ns := range request.Tx.Namespaces {
		if ns.NsId == types.ConfigNamespaceID {
			// Configuration TX is not signed in the same manner as application TX.
			// Its signatures are verified by the ordering service.
			continue
		}
		nsVerifier, ok := verifiers[ns.NsId]
		if !ok {
			logger.Debugf("No verifier for namespace: '%v'", ns.NsId)
			response.Status = protoblocktx.Status_ABORTED_SIGNATURE_INVALID
			return response
		}

		// NOTE: We do not compare the namespace version in the transaction
		//       against the namespace version in the verifier. This is because if
		//       the versions mismatch, and we reject the transaction, the coordinator
		//       would mark the transaction as invalid due to a bad signature. However,
		//       this may not be true if the policy was not actually updated with the
		//       new version. Hence, we should proceed to validate the signatures. If
		//       the signatures are valid, the validator-committer service would
		//       still mark the transaction as invalid due to an MVCC conflict on the
		//       namespace version, which would reflect the correct validation status.
		if err := nsVerifier.VerifyNs(request.Tx, nsIndex); err != nil {
			logger.Debugf("Invalid signature found: '%v', namespace id: '%v'", request.Tx.Id, ns.NsId)
			response.Status = protoblocktx.Status_ABORTED_SIGNATURE_INVALID
			return response
		}
	}
	return response
}
