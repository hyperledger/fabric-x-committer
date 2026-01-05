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
	"github.com/hyperledger/fabric-lib-go/bccsp/factory"
	"github.com/hyperledger/fabric-x-common/common/channelconfig"
	"github.com/hyperledger/fabric-x-common/msp"
	"github.com/hyperledger/fabric-x-common/protoutil"

	"github.com/hyperledger/fabric-x-committer/api/committerpb"
	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/service/verifier/policy"
	"github.com/hyperledger/fabric-x-committer/utils"
	"github.com/hyperledger/fabric-x-committer/utils/signature"
)

type verifier struct {
	verifiers atomic.Pointer[map[string]*signature.NsVerifier]
	bundle    *channelconfig.Bundle
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
	update *servicepb.VerifierUpdates,
) error {
	if update == nil || (update.Config == nil && update.NamespacePolicies == nil) {
		return nil
	}

	if err := v.updateBundle(update); err != nil {
		return err
	}

	// We parse the policy during validation and mark transactions as invalid if parsing fails.
	// While it is unlikely that policy parsing would fail at this stage, it could happen
	// if the stored policy in the database is corrupted or maliciously altered, or if there is a
	// bug in the committer that modifies the policy bytes.
	idDeserializer := v.bundle.MSPManager()
	newVerifiers, err := createVerifiers(update, idDeserializer)
	if err != nil {
		return errors.Join(ErrUpdatePolicies, err)
	}

	defer logger.Infof("New verification policies for namespaces %v", slices.Collect(maps.Keys(newVerifiers)))

	for k, nsVerifier := range *v.verifiers.Load() {
		_, ok := newVerifiers[k]
		if ok {
			continue
		}

		// If there is a config update, the verifier for signature policies must be
		// updated to use the latest MSP Manager from the new configuration.
		if update.Config != nil {
			err = nsVerifier.UpdateIdentities(idDeserializer)
			if err != nil {
				return err
			}
		}
		newVerifiers[k] = nsVerifier
	}
	v.verifiers.Store(&newVerifiers)
	return nil
}

func createVerifiers(
	update *servicepb.VerifierUpdates, idDeserializer msp.IdentityDeserializer,
) (map[string]*signature.NsVerifier, error) {
	newPolicies := make(map[string]*signature.NsVerifier)
	if update.Config != nil {
		// TODO: Support signature rule for meta namespace policy.
		nsVerifier, err := policy.ParsePolicyFromConfigTx(update.Config.Envelope)
		if err != nil {
			return nil, errors.Join(ErrUpdatePolicies, err)
		}
		newPolicies[committerpb.MetaNamespaceID] = nsVerifier
	}
	if update.NamespacePolicies != nil {
		for _, pd := range update.NamespacePolicies.Policies {
			nsVerifier, err := policy.CreateNamespaceVerifier(pd, idDeserializer)
			if err != nil {
				return nil, errors.Join(ErrUpdatePolicies, err)
			}
			newPolicies[pd.Namespace] = nsVerifier
		}
	}
	return newPolicies, nil
}

func (v *verifier) updateBundle(u *servicepb.VerifierUpdates) error {
	if u.Config == nil {
		return nil
	}
	envelope, err := protoutil.UnmarshalEnvelope(u.Config.Envelope)
	if err != nil {
		return errors.Wrap(err, "error unmarshalling envelope")
	}
	bundle, err := channelconfig.NewBundleFromEnvelope(envelope, factory.GetDefault())
	if err != nil {
		return errors.Wrap(err, "error parsing config")
	}

	v.bundle = bundle
	return nil
}

func (v *verifier) verifyRequest(tx *servicepb.VerifierTx) *servicepb.VerifierResponse {
	logger.Debugf("Validating TX: %s", &utils.LazyJSON{O: tx})
	response := &servicepb.VerifierResponse{
		Ref:    tx.Ref,
		Status: committerpb.Status_COMMITTED,
	}
	// The verifiers might temporarily retain the old map while updatePolicies has already set a new one.
	// This is acceptable, provided the coordinator sends the validation status to the dependency graph
	// after updating the policies in the verifier.
	// This ensures that dependent data transactions on these updated namespaces always use the map
	// containing the latest policy.
	verifiers := *v.verifiers.Load()
	for nsIndex, ns := range tx.Tx.Namespaces {
		if ns.NsId == committerpb.ConfigNamespaceID {
			// Configuration TX is not signed in the same manner as application TX.
			// Its signatures are verified by the ordering service.
			continue
		}
		nsVerifier, ok := verifiers[ns.NsId]
		if !ok {
			logger.Debugf("No verifier for namespace: '%v'", ns.NsId)
			response.Status = committerpb.Status_ABORTED_SIGNATURE_INVALID
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
		if err := nsVerifier.VerifyNs(tx.Ref.TxId, tx.Tx, nsIndex); err != nil {
			logger.Debugf("Invalid signature found: '%v', NsId: '%v': %+v",
				&utils.LazyJSON{O: tx.Ref}, ns.NsId, err)
			response.Status = committerpb.Status_ABORTED_SIGNATURE_INVALID
			return response
		}
	}
	return response
}
