/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package sigtest_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/utils/logging"
	"github.com/hyperledger/fabric-x-committer/utils/signature"
	"github.com/hyperledger/fabric-x-committer/utils/signature/sigtest"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

func BenchmarkSign(b *testing.B) {
	logging.SetupWithConfig(&logging.Config{Enabled: false})
	for _, scheme := range signature.AllRealSchemes {
		f := sigtest.NewSignatureFactory(scheme)
		sk, _ := f.NewKeys()
		s, err := f.NewSigner(sk)
		require.NoError(b, err)

		b.Run(scheme, func(b *testing.B) {
			txs := workload.GenerateTransactions(b, workload.DefaultProfile(8), b.N)

			resBench := make([][]byte, b.N)
			errBench := make([]error, b.N)

			b.ResetTimer()
			for i, tx := range txs {
				resBench[i], errBench[i] = s.SignNs(tx.Id, tx.Tx, 0)
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
		f := sigtest.NewSignatureFactory(scheme)
		sk, pk := f.NewKeys()
		s, err := f.NewSigner(sk)
		require.NoError(b, err)
		v, err := f.NewVerifier(pk)
		require.NoError(b, err)

		b.Run(scheme, func(b *testing.B) {
			txs := workload.GenerateTransactions(b, workload.DefaultProfile(8), b.N)
			for _, tx := range txs {
				sig, signErr := s.SignNs(tx.Id, tx.Tx, 0)
				require.NoError(b, signErr)
				tx.Tx.Endorsements = test.CreateEndorsementsForThresholdRule(sig)
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
