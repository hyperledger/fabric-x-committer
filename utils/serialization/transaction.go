/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package serialization

import (
	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-lib-go/bccsp/factory"
	"github.com/hyperledger/fabric-x-common/api/applicationpb"
	"github.com/hyperledger/fabric-x-common/common/channelconfig"
	"github.com/hyperledger/fabric-x-common/protoutil"
	"google.golang.org/protobuf/proto"
)

// UnmarshalTx unmarshal data bytes to protoblocktx.Tx.
func UnmarshalTx(data []byte) (*applicationpb.Tx, error) {
	var tx applicationpb.Tx
	if err := UnmarshalTxInto(data, &tx); err != nil {
		return nil, err
	}
	return &tx, nil
}

// UnmarshalTxInto unmarshals data bytes into a caller-provided TX, so a caller decoding many
// transactions can back them with one allocation instead of one each. proto.Unmarshal resets tx
// before decoding, so a reused TX is replaced rather than merged into.
func UnmarshalTxInto(data []byte, tx *applicationpb.Tx) error {
	if err := proto.Unmarshal(data, tx); err != nil {
		return errors.Wrap(err, "failed to unmarshal tx")
	}
	return nil
}

// ExtractAppTLSCAsFromEnvelope parses a Fabric config envelope and extracts
// TLS root CA certificates from application organizations.
// Only application org CAs are needed because the sidecar and query services
// accept connections from application clients, not orderer nodes.
func ExtractAppTLSCAsFromEnvelope(envelopeBytes []byte) ([][]byte, error) {
	envelope, err := protoutil.UnmarshalEnvelope(envelopeBytes)
	if err != nil {
		return nil, err
	}

	bundle, err := channelconfig.NewBundleFromEnvelope(envelope, factory.GetDefault())
	if err != nil {
		return nil, err
	}

	app, ok := bundle.ApplicationConfig()
	if !ok {
		return nil, errors.New("could not find application config in envelope")
	}

	var certs [][]byte
	for _, org := range app.Organizations() {
		certs = append(certs, org.MSP().GetTLSRootCerts()...)
	}

	return certs, nil
}
