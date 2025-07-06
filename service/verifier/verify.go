/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package verifier

import (
	"encoding/json"
	"maps"
	"slices"
	"sync/atomic"
	"unicode/utf8"

	"github.com/cockroachdb/errors"
	"go.uber.org/zap/zapcore"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
	"github.com/hyperledger/fabric-x-committer/api/protosigverifierservice"
	"github.com/hyperledger/fabric-x-committer/api/types"
	"github.com/hyperledger/fabric-x-committer/service/verifier/policy"
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
	newPolicies, err := parsePolicies(update)
	if err != nil {
		return err
	}

	newVerifiers := make(map[string]*signature.NsVerifier, len(newPolicies))
	for ns, key := range newPolicies {
		nsVerifier, err := signature.NewNsVerifier(key.GetScheme(), key.GetPublicKey())
		if err != nil {
			return errors.Join(ErrUpdatePolicies, err)
		}
		newVerifiers[ns] = nsVerifier
	}
	defer logger.Infof("New verification policies for namespaces %v", slices.Collect(maps.Keys(newVerifiers)))

	for k, v := range *v.verifiers.Load() {
		if _, ok := newVerifiers[k]; !ok {
			newVerifiers[k] = v
		}
	}
	v.verifiers.Store(&newVerifiers)
	return nil
}

func parsePolicies(update *protosigverifierservice.Update) (map[string]*protoblocktx.NamespacePolicy, error) {
	newPolicies := make(map[string]*protoblocktx.NamespacePolicy)
	if update.Config != nil {
		key, err := policy.ParsePolicyFromConfigTx(update.Config.Envelope)
		if err != nil {
			return nil, errors.Join(ErrUpdatePolicies, err)
		}
		newPolicies[types.MetaNamespaceID] = key
	}
	if update.NamespacePolicies != nil {
		for _, pd := range update.NamespacePolicies.Policies {
			key, err := policy.ParseNamespacePolicyItem(pd)
			if err != nil {
				return nil, errors.Join(ErrUpdatePolicies, err)
			}
			newPolicies[pd.Namespace] = key
		}
	}
	return newPolicies, nil
}

func (v *verifier) verifyRequest(request *protosigverifierservice.Request) *protosigverifierservice.Response {
	debug(request)
	return &protosigverifierservice.Response{
		TxId:     request.Tx.Id,
		BlockNum: request.BlockNum,
		TxNum:    request.TxNum,
		Status:   v.verifyTX(request.Tx),
	}
}

func (v *verifier) verifyTX(tx *protoblocktx.Tx) protoblocktx.Status {
	if status := verifyTxForm(tx); status != retValid {
		return status
	}

	// The verifiers might temporarily retain the old map while updatePolicies has already set a new one.
	// This is acceptable, provided the coordinator sends the validation status to the dependency graph
	// after updating the policies in the verifier.
	// This ensures that dependent data transactions on these updated namespaces always use the map
	// containing the latest policy.
	verifiers := *v.verifiers.Load()
	for nsIndex, ns := range tx.Namespaces {
		if ns.NsId == types.ConfigNamespaceID {
			continue
		}
		nsVerifier, ok := verifiers[ns.NsId]
		if !ok {
			return protoblocktx.Status_ABORTED_SIGNATURE_INVALID
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
		if err := nsVerifier.VerifyNs(tx, nsIndex); err != nil {
			logger.Debugf("Invalid signature found: %v, namespace id: %v", tx.GetId(), ns.NsId)
			return protoblocktx.Status_ABORTED_SIGNATURE_INVALID
		}
	}

	return retValid
}

func verifyTxForm(tx *protoblocktx.Tx) protoblocktx.Status {
	if tx.Id == "" {
		return protoblocktx.Status_ABORTED_MISSING_TXID
	}

	if !utf8.ValidString(tx.Id) {
		// ASN.1. Marshalling only supports valid UTF8 strings.
		// This case is unlikely as the message received via protobuf message which also only support
		// valid UTF8 strings.
		// Thus, we do not create a designated status for such error.
		return protoblocktx.Status_ABORTED_MISSING_TXID
	}

	if len(tx.Namespaces) == 0 {
		return protoblocktx.Status_ABORTED_EMPTY_NAMESPACES
	}

	if len(tx.Namespaces) != len(tx.Signatures) {
		return protoblocktx.Status_ABORTED_SIGNATURE_INVALID
	}

	nsIDs := make(map[string]any)
	for _, ns := range tx.Namespaces {
		if policy.ValidateNamespaceID(ns.NsId) != nil {
			return protoblocktx.Status_ABORTED_NAMESPACE_ID_INVALID
		}
		if _, ok := nsIDs[ns.NsId]; ok {
			return protoblocktx.Status_ABORTED_DUPLICATE_NAMESPACE
		}

		for _, check := range []func(ns *protoblocktx.TxNamespace) protoblocktx.Status{
			checkNamespaceFormation, checkMetaNamespace, checkConfigNamespace,
		} {
			if status := check(ns); status != retValid {
				return status
			}
		}
		nsIDs[ns.NsId] = nil
	}
	return retValid
}

func checkNamespaceFormation(ns *protoblocktx.TxNamespace) protoblocktx.Status {
	if len(ns.ReadWrites) == 0 && len(ns.BlindWrites) == 0 {
		return protoblocktx.Status_ABORTED_NO_WRITES
	}

	keys := make([][]byte, 0, len(ns.ReadsOnly)+len(ns.ReadWrites)+len(ns.BlindWrites))
	for _, r := range ns.ReadsOnly {
		keys = append(keys, r.Key)
	}
	for _, r := range ns.ReadWrites {
		keys = append(keys, r.Key)
	}
	for _, r := range ns.BlindWrites {
		keys = append(keys, r.Key)
	}

	return checkKeys(keys)
}

func checkMetaNamespace(txNs *protoblocktx.TxNamespace) protoblocktx.Status {
	if txNs.NsId != types.MetaNamespaceID {
		return retValid
	}
	if len(txNs.BlindWrites) > 0 {
		return protoblocktx.Status_ABORTED_BLIND_WRITES_NOT_ALLOWED
	}

	nsUpdate := make(map[string]any)
	u := policy.GetUpdatesFromNamespace(txNs)
	if u == nil {
		return retValid
	}
	for _, pd := range u.NamespacePolicies.Policies {
		_, err := policy.ParseNamespacePolicyItem(pd)
		if err != nil {
			if errors.Is(err, policy.ErrInvalidNamespaceID) {
				return protoblocktx.Status_ABORTED_NAMESPACE_ID_INVALID
			}
			return protoblocktx.Status_ABORTED_NAMESPACE_POLICY_INVALID
		}
		// The meta-namespace is updated only via blind-write.
		if pd.Namespace == types.MetaNamespaceID {
			return protoblocktx.Status_ABORTED_NAMESPACE_POLICY_INVALID
		}
		if _, ok := nsUpdate[pd.Namespace]; ok {
			return protoblocktx.Status_ABORTED_NAMESPACE_POLICY_INVALID
		}
		nsUpdate[pd.Namespace] = nil
	}

	return retValid
}

// checkConfigNamespace theoretically, this method is redundant as a config namespace TX
// is only generated by the sidecar, and never by a user. And its content was already
// verified by the Orderer.
// However, a bug might cause us to commit un-parsable config TX, so we validate it anyway.
// We return ABORTED_NAMESPACE_ID_INVALID for errors as we assume a client submitted a config
// transaction.
func checkConfigNamespace(txNs *protoblocktx.TxNamespace) protoblocktx.Status {
	if txNs.NsId != types.ConfigNamespaceID {
		return retValid
	}
	if len(txNs.ReadWrites) > 0 || len(txNs.BlindWrites) != 1 {
		return protoblocktx.Status_ABORTED_NAMESPACE_ID_INVALID
	}
	if string(txNs.BlindWrites[0].Key) != types.ConfigKey {
		return protoblocktx.Status_ABORTED_NAMESPACE_ID_INVALID
	}
	_, err := policy.ParsePolicyFromConfigTx(txNs.BlindWrites[0].Value)
	if err != nil {
		return protoblocktx.Status_ABORTED_NAMESPACE_ID_INVALID
	}
	return retValid
}

func checkKeys(keys [][]byte) protoblocktx.Status {
	seenKeys := make(map[string]any, len(keys))
	for _, k := range keys {
		if k == nil {
			return protoblocktx.Status_ABORTED_NIL_KEY
		}

		sK := string(k)
		if _, ok := seenKeys[sK]; ok {
			return protoblocktx.Status_ABORTED_DUPLICATE_KEY_IN_READ_WRITE_SET
		}
		seenKeys[sK] = nil
	}

	return retValid
}

func debug(request *protosigverifierservice.Request) {
	if logger.Level() > zapcore.DebugLevel {
		return
	}
	data, err := json.Marshal(request.Tx)
	if err != nil {
		logger.Debugf("Failed to marshal TX [%d:%d]", request.BlockNum, request.TxNum)
		return
	}
	logger.Debugf("Validating TX [%d:%d]:\n%s", request.BlockNum, request.TxNum, string(data))
}
