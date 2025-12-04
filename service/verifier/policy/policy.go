/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package policy

import (
	"regexp"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-lib-go/bccsp/factory"
	"github.com/hyperledger/fabric-x-common/common/channelconfig"
	"github.com/hyperledger/fabric-x-common/msp"
	"github.com/hyperledger/fabric-x-common/protoutil"
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-committer/api/applicationpb"
	"github.com/hyperledger/fabric-x-committer/api/protosigverifierservice"
	"github.com/hyperledger/fabric-x-committer/api/types"
	"github.com/hyperledger/fabric-x-committer/utils/signature"
)

// KeyValue represents any key/value implementation.
type KeyValue interface {
	GetKey() []byte
	GetValue() []byte
}

// maxNamespaceIDLength defines the maximum number of characters allowed for namespace IDs.
// PostgreSQL limits identifiers to NAMEDATALEN-1, where NAMEDATALEL=64.
// The namespace tables have the prefix 'ns_', thus there are 60 characters remaining.
// See: https://www.postgresql.org/docs/current/sql-syntax-lexical.html
const maxNamespaceIDLength = 60

// validNamespaceID describes the allowed characters in a namespace ID.
// The name may contain letters, digits, or underscores.
// PostgreSQL requires the name to begin with a letter. This is ensured by our namespace table prefix.
// In addition, we restrict to lowercase letters as PostgreSQL converts does not distinguish between upper/lower case.
// See: https://www.postgresql.org/docs/current/sql-syntax-lexical.html
// The regexp is wrapped with "^...$" to ensure we only match the entire string (namespace ID).
// We use the flags: i - ignore case, s - single line.
var validNamespaceID = regexp.MustCompile(`^[a-z0-9_]+$`)

// ErrInvalidNamespaceID is returned when the namespace ID cannot be parsed.
var ErrInvalidNamespaceID = errors.New("invalid namespace ID")

// GetUpdatesFromNamespace translates a namespace TX to policy updates.
func GetUpdatesFromNamespace(nsTx *applicationpb.TxNamespace) *protosigverifierservice.Update {
	switch nsTx.NsId {
	case types.MetaNamespaceID:
		pd := make([]*applicationpb.PolicyItem, len(nsTx.ReadWrites))
		for i, rw := range nsTx.ReadWrites {
			pd[i] = &applicationpb.PolicyItem{
				Namespace: string(rw.Key),
				Policy:    rw.Value,
			}
		}
		return &protosigverifierservice.Update{
			NamespacePolicies: &applicationpb.NamespacePolicies{
				Policies: pd,
			},
		}
	case types.ConfigNamespaceID:
		for _, rw := range nsTx.BlindWrites {
			if string(rw.Key) == types.ConfigKey {
				return &protosigverifierservice.Update{
					Config: &applicationpb.ConfigTransaction{
						Envelope: rw.Value,
					},
				}
			}
		}
	}
	return nil
}

// CreateNamespaceVerifier parses policy item to a namespace policy.
func CreateNamespaceVerifier(
	pd *applicationpb.PolicyItem, idDeserializer msp.IdentityDeserializer,
) (*signature.NsVerifier, error) {
	if err := validateNamespaceIDInPolicy(pd.Namespace); err != nil {
		return nil, err
	}

	pol := &applicationpb.NamespacePolicy{}
	if err := proto.Unmarshal(pd.Policy, pol); err != nil {
		return nil, errors.Wrap(err, "failed to unmarshal namepsace policy bytes")
	}

	return signature.NewNsVerifier(pol, idDeserializer)
}

// validateNamespaceIDInPolicy checks that a given namespace fulfills namespace naming conventions.
func validateNamespaceIDInPolicy(nsID string) error {
	// If it matches one of the system's namespaces it is invalid.
	if nsID == types.MetaNamespaceID || nsID == types.ConfigNamespaceID {
		return ErrInvalidNamespaceID
	}

	return ValidateNamespaceID(nsID)
}

// ValidateNamespaceID checkes that a given namespace has the required length and allowed characters.
func ValidateNamespaceID(nsID string) error {
	// length checks.
	if len(nsID) == 0 || len(nsID) > maxNamespaceIDLength {
		return ErrInvalidNamespaceID
	}

	// characters check.
	if !validNamespaceID.MatchString(nsID) {
		return ErrInvalidNamespaceID
	}

	return nil
}

// ParsePolicyFromConfigTx parses the meta namespace policy from a config transaction.
func ParsePolicyFromConfigTx(value []byte) (*signature.NsVerifier, error) {
	envelope, err := protoutil.UnmarshalEnvelope(value)
	if err != nil {
		return nil, errors.Wrap(err, "error unmarshalling envelope")
	}
	bundle, err := channelconfig.NewBundleFromEnvelope(envelope, factory.GetDefault())
	if err != nil {
		return nil, errors.Wrap(err, "error parsing config")
	}
	ac, ok := bundle.ApplicationConfig()
	if !ok {
		return nil, errors.New("application configuration is missing")
	}
	acx, ok := ac.(*channelconfig.ApplicationConfig)
	if !ok {
		return nil, errors.New("application configuration of incorrect type")
	}
	key := acx.MetaNamespaceVerificationKey()
	// We use existing proto here to avoid introducing new ones.
	// So we encode the key schema as the identifier.
	// This will be replaced in the future with a generic policy mechanism.

	return signature.NewNsVerifier(&applicationpb.NamespacePolicy{
		Rule: &applicationpb.NamespacePolicy_ThresholdRule{
			ThresholdRule: &applicationpb.ThresholdRule{
				Scheme:    key.KeyIdentifier,
				PublicKey: key.KeyMaterial,
			},
		},
	}, nil)
}
