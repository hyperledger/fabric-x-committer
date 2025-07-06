/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package sigtest_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/utils/signature"
	"github.com/hyperledger/fabric-x-committer/utils/signature/sigtest"
)

func BenchmarkSign(b *testing.B) {
	g := workload.StartGenerator(b, workload.DefaultProfile(8))
	for _, scheme := range signature.AllRealSchemes {
		f := sigtest.NewSignatureFactory(scheme)
		sk, _ := f.NewKeys()
		s, err := f.NewSigner(sk)
		require.NoError(b, err)

		b.Run(scheme, func(b *testing.B) {
			txs := g.NextN(b.Context(), b.N)

			var (
				resBench []byte
				errBench error
			)

			b.ResetTimer()
			for _, tx := range txs {
				resBench, errBench = s.SignNs(tx, 0)
			}
			b.StopTimer()
			require.NoError(b, errBench)
			require.NotNil(b, resBench)
		})
	}
}

func BenchmarkVerify(b *testing.B) {
	g := workload.StartGenerator(b, workload.DefaultProfile(8))
	for _, scheme := range signature.AllRealSchemes {
		f := sigtest.NewSignatureFactory(scheme)
		sk, pk := f.NewKeys()
		s, err := f.NewSigner(sk)
		require.NoError(b, err)
		v, err := f.NewVerifier(pk)
		require.NoError(b, err)

		b.Run(scheme, func(b *testing.B) {
			txs := g.NextN(b.Context(), b.N)
			for _, tx := range txs {
				sig, _ := s.SignNs(tx, 0)
				tx.Signatures = append(tx.Signatures, sig)
			}

			var errBench error

			b.ResetTimer()
			for _, tx := range txs {
				errBench = v.VerifyNs(tx, 0)
			}
			b.StopTimer()
			require.NoError(b, errBench)
		})
	}
}
