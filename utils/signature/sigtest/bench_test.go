/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package sigtest_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/api/applicationpb"
	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/utils/logging"
	"github.com/hyperledger/fabric-x-committer/utils/signature"
	"github.com/hyperledger/fabric-x-committer/utils/signature/sigtest"
)

func BenchmarkDigest(b *testing.B) {
	logging.SetupWithConfig(&logging.Config{Enabled: false})
	txs := workload.GenerateTransactions(b, workload.DefaultProfile(8), b.N)

	resBench := make([][]byte, b.N)
	errBench := make([]error, b.N)

	b.ResetTimer()
	for i, tx := range txs {
		resBench[i], errBench[i] = signature.DigestTxNamespace(tx.Id, tx.Tx.Namespaces[0])
	}
	b.StopTimer()
	for i := range b.N {
		require.NoError(b, errBench[i], "error at index %d", i)
		require.NotNil(b, resBench[i], "no result at index %d", i)
	}
}

func BenchmarkSign(b *testing.B) {
	logging.SetupWithConfig(&logging.Config{Enabled: false})
	for _, scheme := range signature.AllRealSchemes {
		sk, _ := sigtest.NewKeyPair(scheme)
		endorser, err := sigtest.NewNsEndorserFromKey(scheme, sk)
		require.NoError(b, err)

		b.Run(scheme, func(b *testing.B) {
			txs := workload.GenerateTransactions(b, workload.DefaultProfile(8), b.N)

			resBench := make([]*applicationpb.Endorsements, b.N)
			errBench := make([]error, b.N)

			b.ResetTimer()
			for i, tx := range txs {
				resBench[i], errBench[i] = endorser.EndorseTxNs(tx.Id, tx.Tx, 0)
			}
			b.StopTimer()
			for i := range b.N {
				require.NoError(b, errBench[i], "error at index %d", i)
				require.NotNil(b, resBench[i], "no result at index %d", i)
			}
		})
	}
}

func BenchmarkVerify(b *testing.B) {
	logging.SetupWithConfig(&logging.Config{Enabled: false})
	for _, scheme := range signature.AllRealSchemes {
		sk, pk := sigtest.NewKeyPair(scheme)
		endorser, err := sigtest.NewNsEndorserFromKey(scheme, sk)
		require.NoError(b, err)
		v, err := sigtest.NewNsVerifierFromKey(scheme, pk)
		require.NoError(b, err)

		b.Run(scheme, func(b *testing.B) {
			txs := workload.GenerateTransactions(b, workload.DefaultProfile(8), b.N)
			for _, tx := range txs {
				endorsement, endorsementErr := endorser.EndorseTxNs(tx.Id, tx.Tx, 0)
				require.NoError(b, endorsementErr)
				tx.Tx.Endorsements = []*applicationpb.Endorsements{endorsement}
			}

			errBench := make([]error, b.N)

			b.ResetTimer()
			for i, tx := range txs {
				errBench[i] = v.VerifyNs(tx.Id, tx.Tx, 0)
			}
			b.StopTimer()
			for i := range b.N {
				require.NoError(b, errBench[i], "error at index %d", i)
			}
		})
	}
}
