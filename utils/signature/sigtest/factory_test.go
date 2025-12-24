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

	"github.com/hyperledger/fabric-x-committer/api/applicationpb"
	"github.com/hyperledger/fabric-x-committer/utils/signature"
)

func TestEndToEnd(t *testing.T) {
	t.Parallel()
	for _, scheme := range signature.AllSchemes {
		t.Run(scheme, func(t *testing.T) {
			t.Parallel()
			priv, pub := NewKeyPair(scheme)
			v, err := signature.NewNsVerifierFromKey(scheme, pub)
			require.NoError(t, err)
			e, err := NewNsEndorserFromKey(scheme, priv)
			require.NoError(t, err)
			txID := "test"
			tx := &applicationpb.Tx{
				Namespaces: []*applicationpb.TxNamespace{{
					NsId:       "0",
					NsVersion:  0,
					ReadWrites: make([]*applicationpb.ReadWrite, 0),
				}},
			}
			endorsement, err := e.EndorseTxNs(txID, tx, 0)
			tx.Endorsements = []*applicationpb.Endorsements{endorsement}
			require.NoError(t, err)
			require.NoError(t, v.VerifyNs(txID, tx, 0))
		})
	}
}

func TestEcdsaPem(t *testing.T) {
	t.Parallel()
	// Currently, only ECDSA is encoded to PEM, so we only test it.
	scheme := signature.Ecdsa
	dir := t.TempDir()
	pemPath := filepath.Join(dir, fmt.Sprintf("%s.pem", signature.Ecdsa))
	priv, pub := NewKeyPair(scheme)
	require.NoError(t, os.WriteFile(pemPath, append(priv, pub...), 0o600))

	v, err := signature.NewNsVerifierFromKey(scheme, pub)
	require.NoError(t, err)
	e, err := NewNsEndorserFromKey(scheme, priv)
	require.NoError(t, err)

	m, err := readPem(pemPath)
	require.NoError(t, err)

	var pemV *signature.NsVerifier
	var pemS *NsEndorser

	for key, value := range m {
		t.Log(key)
		if strings.Contains(strings.ToLower(key), "public") {
			pemV, err = signature.NewNsVerifierFromKey(scheme, value)
			require.NoError(t, err)
		}
		if strings.Contains(strings.ToLower(key), "private") {
			pemS, err = NewNsEndorserFromKey(scheme, value)
			require.NoError(t, err)
		}
	}

	require.NotNil(t, pemV, "missing public key in PEM")
	require.NotNil(t, pemS, "missing private key in PEM")

	txID := "test"
	tx := &applicationpb.Tx{
		Namespaces: []*applicationpb.TxNamespace{{
			NsId:       "0",
			NsVersion:  0,
			ReadWrites: make([]*applicationpb.ReadWrite, 0),
		}},
	}

	endorsement, err := e.EndorseTxNs(txID, tx, 0)
	require.NoError(t, err)
	tx.Endorsements = []*applicationpb.Endorsements{endorsement}
	require.NoError(t, pemV.VerifyNs(txID, tx, 0))

	endorsement, err = pemS.EndorseTxNs(txID, tx, 0)
	require.NoError(t, err)
	tx.Endorsements = []*applicationpb.Endorsements{endorsement}
	require.NoError(t, v.VerifyNs(txID, tx, 0))
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
