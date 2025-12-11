/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package workload

import (
	"crypto/rand"
	"io"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-x-common/common/crypto"
	"github.com/hyperledger/fabric-x-common/protoutil"

	"github.com/hyperledger/fabric-x-committer/api/applicationpb"
	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/utils"
	"github.com/hyperledger/fabric-x-committer/utils/ordererconn"
)

// TxBuilder is a convenient way to create an enveloped TX.
type TxBuilder struct {
	ChannelID   string
	EnvSigner   protoutil.Signer
	TxSigner    *TxSignerVerifier
	EnvCreator  []byte
	NonceSource io.Reader
}

// NewTxBuilderFromPolicy instantiate a TxBuilder from a given policy profile.
func NewTxBuilderFromPolicy(policy *PolicyProfile, nonceSource io.Reader) (*TxBuilder, error) {
	envSigner, err := ordererconn.NewIdentitySigner(policy.Identity)
	if err != nil {
		return nil, err
	}
	var envCreator []byte
	if envSigner != nil {
		envCreator, err = envSigner.Serialize()
		if err != nil {
			return nil, errors.Wrap(err, "error serializing identity creator")
		}
	}
	return &TxBuilder{
		ChannelID:   policy.ChannelID,
		TxSigner:    NewTxSignerVerifier(policy),
		EnvSigner:   envSigner,
		EnvCreator:  envCreator,
		NonceSource: nonceSource,
	}, nil
}

// MakeTx makes an enveloped TX with the builder's properties.
func (txb *TxBuilder) MakeTx(tx *applicationpb.Tx) *servicepb.LoadGenTx {
	return txb.makeTx(nil, tx)
}

// MakeTxWithID makes an enveloped TX with the builder's properties.
// It uses the given TX-ID instead of generating a valid one.
func (txb *TxBuilder) MakeTxWithID(txID string, tx *applicationpb.Tx) *servicepb.LoadGenTx {
	return txb.makeTx(&txID, tx)
}

// makeTx does the following steps:
//  1. Generates the signature-header, and TX-ID.
//  2. Signs the TX:
//     - If the TX already have a signature, it doesn't re-sign it.
//     - If TxSigner is given, it is used to sign the TX.
//     - Otherwise, it puts empty signatures for all namespaces to ensure well-formed TX.
//  3. Serializes the envelope's payload.
//  4. Signs the payload (if EnvSigner is given).
//  5. Serializes the envelope.
//
// Returns a [protoloadgen.TX] with the appropriate values.
func (txb *TxBuilder) makeTx(optionalTxID *string, blockTx *applicationpb.Tx) *servicepb.LoadGenTx {
	//  1. Generates the signature-header, and TX-ID.
	sigHeader := &common.SignatureHeader{
		Creator: txb.EnvCreator,
		Nonce:   readNonce(txb.NonceSource),
	}
	var txID string
	if optionalTxID == nil {
		txID = protoutil.ComputeTxID(sigHeader.Nonce, sigHeader.Creator)
	} else {
		txID = *optionalTxID
	}

	//  2. Signs the TX:
	switch {
	case len(blockTx.Endorsements) > 0:
		// If the TX already have a signature, it doesn't re-sign it.
	case txb.TxSigner != nil:
		// If TxSigner is given, it is used to sign the TX.
		txb.TxSigner.Sign(txID, blockTx)
	case txb.TxSigner == nil:
		// Otherwise, it puts empty signatures for all namespaces to ensure well-formed TX.
		blockTx.Endorsements = make([]*applicationpb.Endorsements, len(blockTx.Namespaces))
	}

	//  3. Serializes the envelope's payload.
	tx := &servicepb.LoadGenTx{
		Id: txID,
		Tx: blockTx,
	}
	chanHeader := protoutil.MakeChannelHeader(common.HeaderType_MESSAGE, 0, txb.ChannelID, 0)
	chanHeader.TxId = txID
	tx.EnvelopePayload = protoutil.MarshalOrPanic(&common.Payload{
		Header: protoutil.MakePayloadHeader(chanHeader, sigHeader),
		Data:   protoutil.MarshalOrPanic(blockTx),
	})

	//  4. Signs the payload (if EnvSigner is given).
	if txb.EnvSigner != nil {
		var err error
		tx.EnvelopeSignature, err = txb.EnvSigner.Sign(tx.EnvelopePayload)
		utils.Must(errors.Wrap(err, "error signing payload"))
	}

	//  5. Serializes the envelope.
	tx.SerializedEnvelope = protoutil.MarshalOrPanic(&common.Envelope{
		Payload:   tx.EnvelopePayload,
		Signature: tx.EnvelopeSignature,
	})
	return tx
}

// readNonce reads a nonce from the given source.
// If no source is given, it uses "crypto/rand" as a source.
func readNonce(source io.Reader) []byte {
	if source == nil {
		source = rand.Reader
	}
	return utils.MustRead(source, crypto.NonceSize)
}
