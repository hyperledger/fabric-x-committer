/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package main

import (
	_ "embed"
	"errors"
	"fmt"
	"testing"

	"github.com/hyperledger/fabric-x-committer/cmd/cliutil"
	"github.com/hyperledger/fabric-x-committer/cmd/config"
)

//nolint:paralleltest // Cannot parallelize due to logger.
func TestMockCMD(t *testing.T) {
	s := cliutil.StartDefaultSystem(t)
	s.Services.Orderer[0] = s.ThisService
	commonTests := []cliutil.CommandTest{
		{
			Name:         "print version",
			Args:         []string{"version"},
			CmdStdOutput: cliutil.FullCommitterVersion(),
		},
		{
			Name: "trailing flag args for version",
			Args: []string{"version", "--test"},
			Err:  errors.New("unknown flag: --test"),
		},
		{
			Name: "trailing command args for version",
			Args: []string{"version", "test"},
			Err:  fmt.Errorf(`unknown command "test" for "%v version"`, mockCmdName),
		},
	}
	for _, test := range commonTests {
		tc := test
		t.Run(test.Name, func(t *testing.T) {
			cliutil.UnitTestRunner(t, mockCMD(), tc)
		})
	}

	for _, serviceCase := range []struct {
		Command  []string
		Name     string
		Template string
	}{
		{Command: []string{"start", "vc"}, Name: mockVcName, Template: config.TemplateVC},
		{Command: []string{"start", "verifier"}, Name: mockVerifierName, Template: config.TemplateVerifier},
		{Command: []string{"start", "orderer"}, Name: mockOrdererName, Template: config.TemplateMockOrderer},
	} {
		t.Run(serviceCase.Name, func(t *testing.T) {
			cases := []cliutil.CommandTest{
				{
					Name:              "start",
					Args:              serviceCase.Command,
					CmdLoggerOutputs:  []string{"Serving", s.ThisService.GrpcEndpoint.String()},
					CmdStdOutput:      fmt.Sprintf("Starting %v", serviceCase.Name),
					UseConfigTemplate: serviceCase.Template,
					System:            s,
				},
			}
			for _, test := range cases {
				tc := test
				t.Run(test.Name, func(t *testing.T) {
					cliutil.UnitTestRunner(t, mockCMD(), tc)
				})
			}
		})
	}
}
