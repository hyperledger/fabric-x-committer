/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package sigtest

import (
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
	"github.com/hyperledger/fabric-x-committer/utils/signature"
)

func TestEndToEnd(t *testing.T) {
	t.Parallel()
	for _, schema := range signature.AllSchemes {
		t.Run(schema, func(t *testing.T) {
			t.Parallel()
			f := NewSignatureFactory(schema)
			priv, pub := f.NewKeys()
			v, err := f.NewVerifier(pub)
			require.NoError(t, err)
			s, err := f.NewSigner(priv)
			require.NoError(t, err)
			tx := protoblocktx.Tx{
				Id: "test",
				Namespaces: []*protoblocktx.TxNamespace{
					{
						NsId:       "0",
						NsVersion:  0,
						ReadWrites: make([]*protoblocktx.ReadWrite, 0),
					},
				},
			}
			sig, err := s.SignNs(&tx, 0)
			tx.Signatures = [][]byte{sig}
			require.NoError(t, err)
			require.NoError(t, v.VerifyNs(&tx, 0))
		})
	}
}

func TestEcdsaPem(t *testing.T) {
	t.Parallel()
	// Currently, only ECDSA is encoded to PEM, so we only test it.
	f := NewSignatureFactory(signature.Ecdsa)
	dir := t.TempDir()
	pemPath := filepath.Join(dir, fmt.Sprintf("%s.pem", signature.Ecdsa))
	priv, pub := f.NewKeys()
	require.NoError(t, os.WriteFile(pemPath, append(priv, pub...), 0o600))

	v, err := f.NewVerifier(pub)
	require.NoError(t, err)
	s, err := f.NewSigner(priv)
	require.NoError(t, err)

	m, err := readPem(pemPath)
	require.NoError(t, err)

	var pemV *signature.NsVerifier
	var pemS *NsSigner

	for key, value := range m {
		t.Log(key)
		if strings.Contains(strings.ToLower(key), "public") {
			pemV, err = f.NewVerifier(value)
			require.NoError(t, err)
		}
		if strings.Contains(strings.ToLower(key), "private") {
			pemS, err = f.NewSigner(value)
			require.NoError(t, err)
		}
	}

	require.NotNil(t, pemV, "missing public key in PEM")
	require.NotNil(t, pemS, "missing private key in PEM")

	tx := protoblocktx.Tx{
		Id: "test",
		Namespaces: []*protoblocktx.TxNamespace{
			{
				NsId:       "0",
				NsVersion:  0,
				ReadWrites: make([]*protoblocktx.ReadWrite, 0),
			},
		},
	}

	sig, err := s.SignNs(&tx, 0)
	require.NoError(t, err)
	tx.Signatures = [][]byte{sig}
	require.NoError(t, pemV.VerifyNs(&tx, 0))

	sig, err = pemS.SignNs(&tx, 0)
	require.NoError(t, err)
	tx.Signatures = [][]byte{sig}
	require.NoError(t, v.VerifyNs(&tx, 0))
}

func readPem(certPath string) (map[string][]byte, error) {
	pemContent, err := os.ReadFile(certPath)
	if err != nil {
		return nil, err
	}

	ret := make(map[string][]byte)
	for {
		block, rest := pem.Decode(pemContent)
		if block == nil {
			break
		}
		pemContent = rest
		ret[block.Type] = pem.EncodeToMemory(block)
	}
	return ret, nil
}
