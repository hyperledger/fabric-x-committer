/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package main

import (
	"github.com/cockroachdb/errors"

	"github.com/hyperledger/fabric-x-committer/cmd/config"
	"github.com/hyperledger/fabric-x-committer/utils/serve"
)

const (
	sidecarService     = "sidecar"
	coordinatorService = "coordinator"
	vcService          = "vc"
	verifierService    = "verifier"
	queryService       = "query"
)

var serviceNames = map[string]string{
	sidecarService:     "Sidecar",
	coordinatorService: "Coordinator",
	vcService:          "Validator-Committer",
	verifierService:    "Verifier",
	queryService:       "Query-Service",
}

func readConfig(name, configPath string) (any, *serve.Config, error) {
	switch name {
	case sidecarService:
		return config.ReadSidecarYamlAndSetupLogging(config.NewViperWithSidecarDefaults(), configPath)
	case coordinatorService:
		return config.ReadCoordinatorYamlAndSetupLogging(config.NewViperWithCoordinatorDefaults(), configPath)
	case vcService:
		return config.ReadVCYamlAndSetupLogging(config.NewViperWithVCDefaults(), configPath)
	case verifierService:
		return config.ReadVerifierYamlAndSetupLogging(config.NewViperWithVerifierDefaults(), configPath)
	case queryService:
		return config.ReadQueryYamlAndSetupLogging(config.NewViperWithQueryDefaults(), configPath)
	default:
		return nil, nil, errors.Newf("unknown service: %s", name)
	}
}
