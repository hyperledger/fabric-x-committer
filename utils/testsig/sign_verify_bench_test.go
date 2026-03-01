/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package testsig_test

import (
	"crypto/sha256"
	"testing"

	"github.com/hyperledger/fabric-lib-go/bccsp/factory"
	"github.com/hyperledger/fabric-lib-go/common/flogging"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-x-common/common/channelconfig"
	"github.com/hyperledger/fabric-x-common/protoutil"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/utils/signature"
)

var testedSchemes = append(signature.AllRealSchemes, "MSP")

func BenchmarkMarshal(b *testing.B) {
	flogging.Init(flogging.Config{LogSpec: "fatal"})
	txs := workload.GenerateTransactions(b, workload.DefaultProfile(8), b.N)

	resBench := make([][]byte, b.N)
	errBench := make([]error, b.N)

	b.ResetTimer()
	for i, tx := range txs {
		resBench[i], errBench[i] = tx.Tx.Namespaces[0].ASN1Marshal(tx.Id)
	}
	b.StopTimer()
	for i := range b.N {
		require.NoError(b, errBench[i], "error at index %d", i)
		require.NotNil(b, resBench[i], "no result at index %d", i)
	}
}

func BenchmarkDigest(b *testing.B) {
	flogging.Init(flogging.Config{LogSpec: "fatal"})
	txs := workload.GenerateTransactions(b, workload.DefaultProfile(8), b.N)

	resBench := make([][]byte, b.N)
	errBench := make([]error, b.N)

	b.ResetTimer()
	for i, tx := range txs {
		resBench[i], errBench[i] = tx.Tx.Namespaces[0].ASN1Marshal(tx.Id)
		d := sha256.Sum256(resBench[i])
		resBench[i] = d[:]
	}
	b.StopTimer()
	for i := range b.N {
		require.NoError(b, errBench[i], "error at index %d", i)
		require.NotNil(b, resBench[i], "no result at index %d", i)
	}
}

func BenchmarkSign(b *testing.B) {
	flogging.Init(flogging.Config{LogSpec: "fatal"})
	for _, scheme := range testedSchemes {
		b.Run(scheme, func(b *testing.B) {
			policy, _ := makePolicy(b, scheme)
			endorser := workload.NewTxEndorser(policy)

			// We generate the TXs with a generic policy and add the endorsements as part of the benchmark.
			txs := workload.GenerateTransactions(b, workload.DefaultProfile(8), b.N)

			b.ResetTimer()
			for _, tx := range txs {
				endorser.Endorse(tx.Id, tx.Tx)
			}
			b.StopTimer()

			for i, tx := range txs {
				require.NotEmpty(b, tx.Tx.Endorsements, "endorsement is empty at index %d", i)
			}
		})
	}
}

func BenchmarkVerify(b *testing.B) {
	flogging.Init(flogging.Config{LogSpec: "fatal"})
	for _, scheme := range testedSchemes {
		b.Run(scheme, func(b *testing.B) {
			policy, block := makePolicy(b, scheme)
			envelope, err := protoutil.ExtractEnvelope(block, 0)
			require.NoError(b, err)
			bundle, err := channelconfig.NewBundleFromEnvelope(envelope, factory.GetDefault())
			require.NoError(b, err)

			// We generate the TXs with the given policy endorsements.
			profile := workload.DefaultProfile(8)
			profile.Policy = *policy
			txs := workload.GenerateTransactions(b, profile, b.N)

			allPolicies := workload.NewTxEndorser(policy).VerificationPolicies()
			v, err := signature.NewNsVerifier(allPolicies[workload.DefaultGeneratedNamespaceID], bundle.MSPManager())
			require.NoError(b, err)

			errBench := make([]error, b.N)

			b.ResetTimer()
			for i, tx := range txs {
				errBench[i] = v.VerifyNs(tx.Id, tx.Tx, 0)
			}
			b.StopTimer()

			for i, curErr := range errBench {
				require.NoError(b, curErr, "error at index %d", i)
			}
		})
	}
}

func makePolicy(b *testing.B, scheme string) (*workload.PolicyProfile, *common.Block) {
	b.Helper()
	policy := &workload.PolicyProfile{
		NamespacePolicies: map[string]*workload.Policy{
			workload.DefaultGeneratedNamespaceID: {
				Scheme: scheme,
				Seed:   10,
			},
		},
		CryptoMaterialPath:    b.TempDir(),
		PeerOrganizationCount: 3,
	}
	block, err := workload.CreateOrExtendConfigBlockWithCrypto(policy)
	require.NoError(b, err)
	return policy, block
}
