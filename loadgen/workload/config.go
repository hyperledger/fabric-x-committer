/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package workload

import (
	"strings"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	commontypes "github.com/hyperledger/fabric-x-common/api/types"

	"github.com/hyperledger/fabric-x-committer/utils/ordererdial"
	"github.com/hyperledger/fabric-x-committer/utils/signature"
)

// Defines Policy.Scheme.
const (
	PolicySchemeMSP         = "MSP"
	PolicySchemeDefault     = PolicySchemeMSP
	PolicySchemeUnspecified = ""
)

// Probability is a float in the closed interval [0,1].
type Probability = float64

// Profile describes the generated workload characteristics.
// It only contains parameters that deterministically affect the
// generated items.
// The items order, however, might be affected by other parameters.
type Profile struct {
	Block       BlockProfile       `mapstructure:"block" yaml:"block"`
	Transaction TransactionProfile `mapstructure:"transaction" yaml:"transaction"`
	Policy      PolicyProfile      `mapstructure:"policy" yaml:"policy"`

	// Seed is the single PRF root for the whole workload. Every generated item (keys, values, nonce,
	// metadata, and the new-vs-existing layout) is a pure function of this seed and the item's global
	// transaction index, so the same Seed reproduces the same items.
	Seed int64 `mapstructure:"seed" yaml:"seed"`

	// Workers is the number of parallel producers. They share one global transaction-index counter and
	// the same Seed, so the multiset of generated transactions is independent of the worker count —
	// workers are pure parallelism. The count therefore does not need to be preserved between runs to
	// reproduce items.
	Workers uint32 `mapstructure:"workers" yaml:"workers"`
}

// BlockProfile describes generate block characteristics
// (when applying load to the VC or Verifier, blocks are translated to batches).
// The generated block size is aimed to be MaxSize, if the generated
// TXs rate is sufficient.
// If the generated TXs rate is too low, the block size might
// be less than MaxSize, but at least MinSize.
// In such case, the block is generated at a preferred rate of PreferredRate.
// Blocks wait up to PreferredRate (default: 1 second) before submission.
// If a full block is not ready by then, a partial block is
// submitted if it meets MinSize (default: 1);
// otherwise, the system waits until MinSize is available.
// If the MaxSize is less than or equal to MinSize, PreferredRate is ignored.
type BlockProfile struct {
	MaxSize       uint64        `mapstructure:"max-size" yaml:"max-size"`
	MinSize       uint64        `mapstructure:"min-size" yaml:"min-size"`
	PreferredRate time.Duration `mapstructure:"preferred-rate" yaml:"preferred-rate"`
}

// TransactionProfile describes generate TX characteristics.
type TransactionProfile struct {
	// The byte sizes of the generated key/values/metadata (size=0 => nil), ordered key, value, metadata.
	KeySize             uint32 `mapstructure:"key-size" yaml:"key-size"`
	ReadWriteValueSize  uint32 `mapstructure:"read-write-value-size" yaml:"read-write-value-size"`
	BlindWriteValueSize uint32 `mapstructure:"blind-write-value-size" yaml:"blind-write-value-size"`
	MetadataSize        uint32 `mapstructure:"metadata-size" yaml:"metadata-size"`

	// The number of keys to generate (read ver=nil)
	ReadOnlyCount uint32 `mapstructure:"read-only-count" yaml:"read-only-count"`
	// The number of keys to generate (read ver=nil/write)
	ReadWriteCount uint32 `mapstructure:"read-write-count" yaml:"read-write-count"`
	// The number of keys to generate (write)
	BlindWriteCount uint32 `mapstructure:"write-count" yaml:"write-count"`

	// The transaction layout above is fixed: every transaction has the same slot counts. The three knobs
	// below control only which keys fill those slots — fresh keys, or references to keys from earlier
	// transactions — which is what produces (or avoids) commit-time contention. Reads carry nil versions
	// for now; a later PR adds a querier that fills the real versions.

	// NewKeysRate is the average number of new keys generated per transaction; the remaining slots
	// reference keys from earlier transactions, so keys are reused and the coordinator sees commit-time
	// contention. New keys fill slots in layout order: read-write, then blind-write, then read-only.
	// Unset (default): every slot gets a fresh unique key — no reuse, no contention. To create conflicts,
	// set it below the total slot count (read-only + read-write + write); it must not exceed that count.
	// 0 means no new keys at all (every slot references a fixed static set).
	NewKeysRate *float64 `mapstructure:"new-keys-rate" yaml:"new-keys-rate" validate:"omitempty,gte=0"`
	// TxReferenceGap and KeyLookbackWindow together set the window that backward references are drawn from;
	// both are ignored when new-keys-rate is unset. A reference is drawn from the KeyLookbackWindow newest
	// keys that existed TxReferenceGap transactions ago.
	//
	// TxReferenceGap is how far back, in transactions, references reach. 0 (default) draws the newest keys,
	// which may still be in flight — so conflicting transactions can land in the same block (a live
	// dependency); larger values draw older, already-committed keys.
	TxReferenceGap uint64 `mapstructure:"tx-reference-gap" yaml:"tx-reference-gap"`
	// KeyLookbackWindow is how many of the newest keys a reference is drawn from. A larger window spreads
	// references over more keys (less contention); a smaller one concentrates them (more contention). Must
	// be at least the total slot count so a transaction's references stay distinct.
	KeyLookbackWindow uint64 `mapstructure:"key-lookback-window" yaml:"key-lookback-window"`

	// InvalidSignatures is the probability [0,1] that a transaction is stamped with a bad signature
	// (default: 0). The decision is derived deterministically from the transaction index.
	InvalidSignatures Probability `mapstructure:"invalid-signatures" yaml:"invalid-signatures" validate:"gte=0,lte=1"`
}

// Validate checks the split configuration. An unset new-keys-rate keeps the historical fresh-key
// workload (tx-reference-gap and key-lookback-window are ignored). When set, the rate must not exceed the
// total slot count (so the per-transaction new-key count never overflows the transaction's slots), and
// the lookback window must be at least the total slot count so every reference within a transaction is
// distinct.
func (p *TransactionProfile) Validate() error {
	if p.NewKeysRate == nil {
		return nil
	}
	totalSlots := uint64(p.ReadOnlyCount) + uint64(p.ReadWriteCount) + uint64(p.BlindWriteCount)
	if rate := *p.NewKeysRate; rate > float64(totalSlots) {
		return errors.Newf("new-keys-rate %v exceeds total slots %d (read-only + read-write + write)",
			rate, totalSlots)
	}
	if p.KeyLookbackWindow < totalSlots {
		return errors.Newf("key-lookback-window %d must be at least the total slot count %d so every "+
			"reference within a transaction is distinct", p.KeyLookbackWindow, totalSlots)
	}
	return nil
}

// PolicyProfile holds the policy information for the load generation.
type PolicyProfile struct {
	// NamespacePolicies specifies the namespace policies.
	NamespacePolicies map[string]*Policy `mapstructure:"namespace-policies" yaml:"namespace-policies"`

	// OrdererEndpoints may specify the endpoints to add to the config block.
	// If this field is empty, no endpoints will be configured.
	// If ConfigBlockPath is specified, this value is ignored.
	OrdererEndpoints []*commontypes.OrdererEndpoint `mapstructure:"orderer-endpoints" yaml:"orderer-endpoints"`

	// ArtifactsPath may specify the path to the artifacts generated by CreateOrExtendConfigBlockWithCrypto().
	// If this field is empty, the artifacts will be generated into a temporary folder.
	// If this path does not exist, or it is empty, the artifacts will be generated into it.
	// The config block will be fetched from ArtifactsPath.
	ArtifactsPath string `mapstructure:"artifacts-path" yaml:"artifacts-path"`

	// ChannelID and Identity are used to create the TX envelop.
	ChannelID string                      `mapstructure:"channel-id"`
	Identity  *ordererdial.IdentityConfig `mapstructure:"identity"`

	// PeerOrganizationCount may specify the number of peer organizations to generate if the ArtifactsPath
	// is not provided.
	PeerOrganizationCount uint32 `mapstructure:"peer-organization-count"`
}

// Policy describes how to sign/verify a TX.
// It supports a signing with a raw signing key, or via a local MSP.
// Scheme can be a valid signature schemes (NONE, ECDSA, BLS, or EDDSA) or MSP to indicate using a local MSP.
// When Scheme is not MSP, we generate a key using the given Seed, or loading one if KeyPath is given,
// ignoring MSPIdentities.
// When Scheme is MSP, we load the signing identities from MSPIdentities, ignoring Seed and KeyPath.
// In such case, we use the default rule, which state that all peer organization should sign.
// If MSPIdentities is not provided, we load the signing identities from ArtifactsPath.
type Policy struct {
	Scheme        signature.Scheme              `mapstructure:"scheme" yaml:"scheme"`
	Seed          int64                         `mapstructure:"seed" yaml:"seed"`
	KeyPath       *KeyPath                      `mapstructure:"key-path" yaml:"key-path"`
	MSPIdentities []*ordererdial.IdentityConfig `mapstructure:"msp-identities" yaml:"msp-identities"`
}

// KeyPath describes how to find/generate the signature keys.
type KeyPath struct {
	SigningKey      string `mapstructure:"signing-key" yaml:"signing-key"`
	VerificationKey string `mapstructure:"verification-key" yaml:"verification-key"`
	SignCertificate string `mapstructure:"sign-certificate" yaml:"sign-certificate"`
}

// StreamOptions allows adjustment to the stream rate.
// It only contains parameters that do not affect the produced items.
// However, these parameters might affect the order of the items.
type StreamOptions struct {
	// GenBatch impacts the rate by batching generated items before inserting then the channel.
	// This helps overcome the inherit rate limitation of Go channels.
	GenBatch uint32 `mapstructure:"gen-batch" yaml:"gen-batch"`
	// BuffersSize impact the rate by masking fluctuation in performance.
	BuffersSize int `mapstructure:"buffers-size" yaml:"buffers-size"`
	// RateLimit directly impacts the rate by limiting it.
	// TXs are released at RateLimit (default: unlimited).
	RateLimit uint64 `mapstructure:"rate-limit" yaml:"rate-limit"`
}

// Validate checks that the PolicyProfile does not contain invalid entries.
// System namespace "_config" must not be provided explicitly as it is not a real namespace.
// System namespace "_meta" can be derived from the artifacts' path when given.
// But it can be provided explicitly if desired.
// If provided explicitly, it must use a MSP rule.
func (p *PolicyProfile) Validate() error {
	if _, ok := p.NamespacePolicies[committerpb.ConfigNamespaceID]; ok {
		return errors.Newf("system namespace %q must not be provided in the policy profile",
			committerpb.ConfigNamespaceID)
	}

	if getPolicyScheme(p.NamespacePolicies[committerpb.MetaNamespaceID]) != PolicySchemeMSP {
		return errors.Newf("system namespace %q must use scheme %q", committerpb.MetaNamespaceID, PolicySchemeMSP)
	}
	return nil
}

func getPolicyScheme(policy *Policy) string {
	if policy == nil {
		return PolicySchemeDefault
	}
	scheme := strings.ToUpper(policy.Scheme)
	if scheme == PolicySchemeUnspecified {
		return PolicySchemeDefault
	}
	return scheme
}
