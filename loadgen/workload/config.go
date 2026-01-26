/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package workload

import (
	"fmt"
	"time"

	"go.yaml.in/yaml/v3"

	commontypes "github.com/hyperledger/fabric-x-common/api/types"

	"github.com/hyperledger/fabric-x-committer/utils/ordererconn"
	"github.com/hyperledger/fabric-x-committer/utils/signature"
)

// Profile describes the generated workload characteristics.
// It only contains parameters that deterministically affect the
// generated items.
// The items order, however, might be affected by other parameters.
type Profile struct {
	Block       BlockProfile       `mapstructure:"block" yaml:"block"`
	Key         KeyProfile         `mapstructure:"key" yaml:"key"`
	Transaction TransactionProfile `mapstructure:"transaction" yaml:"transaction"`
	Query       QueryProfile       `mapstructure:"query" yaml:"query"`
	Policy      PolicyProfile      `mapstructure:"policy" yaml:"policy"`
	Conflicts   ConflictProfile    `mapstructure:"conflicts" yaml:"conflicts"`

	// The seed to generate the seeds for each worker
	Seed int64 `mapstructure:"seed" yaml:"seed"`

	// Workers is the number of independent producers.
	// Each worker uses a unique seed that is generated from the main seed.
	// To ensure responsibility of items between runs (e.g., for query)
	// the number of workers must be preserved.
	Workers uint32 `mapstructure:"workers" yaml:"workers"`
}

// KeyProfile describes generated keys characteristics.
type KeyProfile struct {
	// Size is the size of the key to generate.
	Size uint32 `mapstructure:"size" yaml:"size"`
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
	// The sizes of the values to generate (size=0 => value=nil)
	ReadWriteValueSize  uint32 `mapstructure:"read-write-value-size" yaml:"read-write-value-size"`
	BlindWriteValueSize uint32 `mapstructure:"blind-write-value-size" yaml:"blind-write-value-size"`
	// The number of keys to generate (read ver=nil)
	ReadOnlyCount *Distribution `mapstructure:"read-only-count" yaml:"read-only-count"`
	// The number of keys to generate (read ver=nil/write)
	ReadWriteCount *Distribution `mapstructure:"read-write-count" yaml:"read-write-count"`
	// The number of keys to generate (write)
	BlindWriteCount *Distribution `mapstructure:"write-count" yaml:"write-count"`
}

// QueryProfile describes generate query characteristics.
type QueryProfile struct {
	// The number of keys to query.
	QuerySize *Distribution `mapstructure:"query-size" yaml:"query-size"`
	// The minimal portion of invalid keys (1 => all keys are invalid).
	// This is a lower bound since some valid keys might have failed to commit due to conflicts.
	MinInvalidKeysPortion *Distribution `mapstructure:"min-invalid-keys-portion" yaml:"min-invalid-keys-portion"`
	// If Shuffle=false, the valid keys will be placed first.
	// Otherwise, they will be shuffled.
	Shuffle bool `mapstructure:"shuffle" yaml:"shuffle"`
}

// ConflictProfile describes the TX conflict characteristics.
// Note that each of the conflicts' probabilities are independent bernoulli distributions.
type ConflictProfile struct {
	// Probability of invalid signatures [0,1] (default: 0)
	InvalidSignatures Probability `mapstructure:"invalid-signatures" yaml:"invalid-signatures"`
	// Dependencies list of dependencies
	Dependencies []DependencyDescription `mapstructure:"dependencies" yaml:"dependencies"`
}

// DependencyDescription describes a dependency type.
type DependencyDescription struct {
	// Probability of the dependency type [0,1] (default: 0)
	Probability Probability `mapstructure:"probability" yaml:"probability"`
	// Gap is the distance between the dependent TXs (default: 1)
	Gap *Distribution `mapstructure:"gap" yaml:"gap"`
	// Src dependency "read", "write", or "read-write"
	Src string `mapstructure:"src" yaml:"src"`
	// Dst dependency "read", "write", or "read-write"
	Dst string `mapstructure:"dst" yaml:"dst"`
}

// PolicyProfile holds the policy information for the load generation.
type PolicyProfile struct {
	// NamespacePolicies specifies the namespace policies.
	NamespacePolicies map[string]*Policy `mapstructure:"namespace-policies" yaml:"namespace-policies"`

	// OrdererEndpoints may specify the endpoints to add to the config block.
	// If this field is empty, no endpoints will be configured.
	// If ConfigBlockPath is specified, this value is ignored.
	OrdererEndpoints []*commontypes.OrdererEndpoint `mapstructure:"orderer-endpoints" yaml:"orderer-endpoints"`

	// CryptoMaterialPath may specify the path to the material generated by CreateDefaultConfigBlockWithCrypto().
	// If this field is empty, the material will be generated into a temporary folder.
	// If this path does not exist, or it is empty, the material will be generated into it.
	// If ConfigBlockPath is empty, the config block will be fetched from CryptoMaterialPath.
	// Otherwise, it will be fetched from ConfigBlockPath regardless of CryptoMaterialPath.
	CryptoMaterialPath string `mapstructure:"crypto-material-path" yaml:"crypto-material-path"`
	ConfigBlockPath    string `mapstructure:"config-block-path" yaml:"config-block-path"`

	// ChannelID and Identity are used to create the TX envelop.
	ChannelID string                      `mapstructure:"channel-id"`
	Identity  *ordererconn.IdentityConfig `mapstructure:"identity"`

	// PeerOrganizationCount may specify the number of peer organizations to generate if the CryptoMaterialPath
	// is not provided.
	PeerOrganizationCount uint32 `mapstructure:"peer-organization-count"`
}

// Policy describes how to sign/verify a TX.
// It supports a signing with a raw signing key, or via a local MSP.
// Scheme can be a valid signature schemes (NONE, ECDSA, BLS, or EDDSA) or MSP to indicate using a local MSP.
// When Scheme is not MSP, we generate a key using the given Seed, or loading one if KeyPath is given.
// When Scheme is MSP, we load the signing identities from the CryptoMaterialPath, ignoring Seed and KeyPath.
// In such case, we use the default rule, which state that all peer organization should sign.
type Policy struct {
	Scheme  signature.Scheme `mapstructure:"scheme" yaml:"scheme"`
	Seed    int64            `mapstructure:"seed" yaml:"seed"`
	KeyPath *KeyPath         `mapstructure:"key-path" yaml:"key-path"`
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

// Debug outputs the profile to stdout.
func (p *Profile) Debug() {
	debug("Profile", p)
}

// Debug outputs the stream configuration to stdout.
func (o *StreamOptions) Debug() {
	debug("Stream Config", o)
}

func debug(title string, val any) {
	d, err := yaml.Marshal(val)
	Must(err)
	fmt.Println("############################################################")
	fmt.Printf("# %s\n", title)
	fmt.Println("############################################################")
	fmt.Println(string(d))
	fmt.Println("############################################################")
}
