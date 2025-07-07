/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package broadcastdeliver

import (
	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric/protoutil"
	"google.golang.org/protobuf/proto"

	"github.com/hyperledger/fabric-x-committer/utils/serialization"
)

type (
	// EnvelopedStream implements broadcast, with the addition of automatically signing the envelope.
	EnvelopedStream struct {
		BroadcastStream
		channelID string
		signer    protoutil.Signer
	}

	// BroadcastStream can be either CFT or BFT broadcast.
	BroadcastStream interface {
		Send(*common.Envelope) error
	}
)

// SendWithEnv wraps a payload with an envelope, signs it and send it to the stream.
func (s *EnvelopedStream) SendWithEnv(dataMsg proto.Message) (string, error) {
	env, txID, err := s.CreateEnvelope(dataMsg)
	if err != nil {
		return txID, err
	}
	err = s.Send(env)
	return txID, errors.Wrap(err, "error submitting envelope")
}

// CreateEnvelope wraps a payload with an envelope, and signs it.
func (s *EnvelopedStream) CreateEnvelope(dataMsg proto.Message) (*common.Envelope, string, error) {
	env, txID, err := serialization.CreateEnvelope(s.channelID, s.signer, dataMsg)
	return env, txID, errors.Wrap(err, "error serializing envelope")
}
