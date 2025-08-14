/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package workload

import (
	cryptornd "crypto/rand"
	mathrnd "math/rand"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-x-common/common/crypto"
	"github.com/hyperledger/fabric-x-common/protoutil"
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-committer/api/protoblocktx"
	"github.com/hyperledger/fabric-x-committer/api/protoloadgen"
	"github.com/hyperledger/fabric-x-committer/utils/ordererconn"
)

type (
	// TxBuilderFactory produce TxBuilder(s) with the the given properties.
	TxBuilderFactory struct {
		NonceRnd  *mathrnd.Rand
		ChannelID string
		EnvSigner protoutil.Signer
		TxSigner  *TxSignerVerifier
	}

	// TxBuilder is a convenient way to create an enveloped TX.
	TxBuilder struct {
		Tx *protoblocktx.Tx

		id              string
		envSigner       protoutil.Signer
		txSigner        *TxSignerVerifier
		channelHeader   *common.ChannelHeader
		signatureHeader *common.SignatureHeader
		payloadHeader   *common.Header
	}
)

// NewTxBuilderFactory instantiate a TxBuilderFactory from a given policy profile.
func NewTxBuilderFactory(policy *PolicyProfile, nonceRnd *mathrnd.Rand) (*TxBuilderFactory, error) {
	envSigner, err := ordererconn.NewIdentitySigner(policy.Identity)
	if err != nil {
		return nil, err
	}
	return &TxBuilderFactory{
		NonceRnd:  nonceRnd,
		ChannelID: policy.ChannelID,
		EnvSigner: envSigner,
		TxSigner:  NewTxSignerVerifier(policy),
	}, nil
}

// New instantiate a TxBuilder with the factory's properties.
func (f *TxBuilderFactory) New() (*TxBuilder, error) {
	signatureHeader, err := newSignatureHeader(f.EnvSigner, f.NonceRnd)
	if err != nil {
		return nil, err
	}
	channelHeader := protoutil.MakeChannelHeader(common.HeaderType_MESSAGE, 0, f.ChannelID, 0)
	txb := &TxBuilder{
		Tx:              &protoblocktx.Tx{},
		envSigner:       f.EnvSigner,
		txSigner:        f.TxSigner,
		signatureHeader: signatureHeader,
		channelHeader:   channelHeader,
	}
	txb.SetTxID(protoutil.ComputeTxID(txb.signatureHeader.Nonce, txb.signatureHeader.Creator))
	return txb, nil
}

// MakeTx is a convenient way to make an enveloped TX in a single line with the factory's properties.
func (f *TxBuilderFactory) MakeTx(tx *protoblocktx.Tx) (*protoloadgen.TX, error) {
	txb, err := f.New()
	if err != nil {
		return nil, err
	}
	txb.Tx = tx
	return txb.Make()
}

// MakeTxWithID is a convenient way to make an enveloped TX in a single line with the factory's properties.
// It overrides the valid TX ID with the given ID.
func (f *TxBuilderFactory) MakeTxWithID(txID string, tx *protoblocktx.Tx) (*protoloadgen.TX, error) {
	txb, err := f.New()
	if err != nil {
		return nil, err
	}
	txb.SetTxID(txID)
	txb.Tx = tx
	return txb.Make()
}

// SetTxID overrides the valid TX ID with the given ID.
func (txb *TxBuilder) SetTxID(txID string) {
	txb.channelHeader.TxId = txID
	txb.payloadHeader = protoutil.MakePayloadHeader(txb.channelHeader, txb.signatureHeader)
	txb.id = txID
}

// Make does the following steps:
//  1. Signs the TX:
//     - If the TX already have a signature, it doesn't re-sign it.
//     - If txSigner is given, it is used to sign the TX.
//     - Otherwise, it puts empty signatures for all namespaces to ensure well-formed TX.
//  2. Generate and serialize the payload.
//  3. Signs the payload (if envSigner is given).
//  4. Generate and serialize the envelope.
//
// Returns a [protoloadgen.TX] with the appropriate values.
func (txb *TxBuilder) Make() (*protoloadgen.TX, error) {
	tx := &protoloadgen.TX{
		Id: txb.id,
		Tx: txb.Tx,
	}

	// We don't re-sign a TX signature if it is already signed to support pre-signed TXs.
	if txb.txSigner != nil && tx.Tx.Signatures == nil {
		txb.txSigner.Sign(txb.id, txb.Tx)
	}
	// If a signer is not supplied, an empty signature will be added to ensure well-formed TX.
	if tx.Tx.Signatures == nil {
		tx.Tx.Signatures = make([][]byte, len(txb.Tx.Namespaces))
	}

	data, err := proto.Marshal(txb.Tx)
	if err != nil {
		return nil, errors.Wrap(err, "error marshaling TX")
	}
	tx.EnvelopePayload, err = protoutil.Marshal(&common.Payload{
		Header: txb.payloadHeader,
		Data:   data,
	})
	if err != nil {
		return nil, errors.Wrap(err, "error marshaling payload")
	}

	if txb.envSigner != nil {
		tx.EnvelopeSignature, err = txb.envSigner.Sign(tx.EnvelopePayload)
		if err != nil {
			return nil, errors.Wrap(err, "error signing payload")
		}
	}

	tx.SerializedEnvelope, err = proto.Marshal(&common.Envelope{
		Payload:   tx.EnvelopePayload,
		Signature: tx.EnvelopeSignature,
	})
	if err != nil {
		return nil, errors.Wrap(err, "error marshaling envelope")
	}
	return tx, nil
}

// newSignatureHeader returns a [common.SignatureHeader] with a valid ID.
func newSignatureHeader(envSigner protoutil.Signer, nonceRnd *mathrnd.Rand) (*common.SignatureHeader, error) {
	header := &common.SignatureHeader{}

	if envSigner != nil {
		var err error
		header.Creator, err = envSigner.Serialize()
		if err != nil {
			return nil, errors.Wrap(err, "error serializing signer")
		}
	}

	header.Nonce = make([]byte, crypto.NonceSize)
	rndReader := cryptornd.Read
	if nonceRnd != nil {
		rndReader = nonceRnd.Read
	}
	n, err := rndReader(header.Nonce)
	if err != nil {
		return nil, errors.Wrap(err, "error getting random nonce")
	}
	if n < crypto.NonceSize {
		return nil, errors.New("random nonce too short")
	}
	return header, nil
}
