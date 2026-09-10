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

	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/cmd/cliutil"
	"github.com/hyperledger/fabric-x-committer/cmd/config"
	"github.com/hyperledger/fabric-x-committer/utils/testdb"
)

//nolint:paralleltest // Cannot parallelize due to logger.
func TestCommitterCMD(t *testing.T) {
	require.NoError(t, testdb.SetupSharedContainer())
	t.Cleanup(testdb.CleanupSharedContainer)

	s := cliutil.StartDefaultSystem(t)
	s.Services.Orderer[0] = s.ThisService

	// The real VC service (start-vc) and query service (start-query)
	// connects to the database on startup, so we override the
	// dummy DB config with a real connection.
	conn := testdb.PrepareTestEnv(t)
	s.DB = config.DatabaseConfig{
		Name:      conn.Database,
		Username:  conn.User,
		Password:  conn.Password,
		Endpoints: conn.Endpoints,
	}
	commonTests := []cliutil.CommandTest{
		{
			Name:         "when version is requested, it prints the version",
			Args:         []string{"version"},
			CmdStdOutput: cliutil.FullCommitterVersion(),
		},
		{
			Name: "when version has an unknown flag, it rejects the flag",
			Args: []string{"version", "--test"},
			Err:  errors.New("unknown flag: --test"),
		},
		{
			Name: "when version has a trailing command, it rejects the command",
			Args: []string{"version", "test"},
			Err:  fmt.Errorf(`unknown command "test" for "%v version"`, cliutil.CommitterName),
		},
		{
			Name: "when migration operations are combined, it rejects the command",
			Args: []string{"--init-from-snapshot", "snapshot.fxgenesis", "--activate-migration", "snapshot.fxgenesis"},
			Err:  errors.New("--init-from-snapshot, --activate-migration, and --verify-migration are mutually exclusive"),
		},
		{
			Name: "when a migration operation is combined with a subcommand, it rejects the command",
			Args: []string{"--init-from-snapshot", "snapshot.fxgenesis", "version"},
			Err:  errors.New("unknown flag: --init-from-snapshot"),
		},
	}
	for _, test := range commonTests {
		tc := test
		t.Run(test.Name, func(t *testing.T) {
			cliutil.UnitTestRunner(t, committerCMD(), tc)
		})
	}

	for _, serviceCase := range []struct {
		Cmd   []string
		Name  string
		Templ string
	}{
		{Cmd: []string{"start", sidecarService}, Name: serviceNames[sidecarService], Templ: config.TemplateSidecar},
		{
			Cmd: []string{"start", coordinatorService}, Name: serviceNames[coordinatorService],
			Templ: config.TemplateCoordinator,
		},
		{Cmd: []string{"start", vcService}, Name: serviceNames[vcService], Templ: config.TemplateVC},
		{Cmd: []string{"start", verifierService}, Name: serviceNames[verifierService], Templ: config.TemplateVerifier},
		{Cmd: []string{"start", queryService}, Name: serviceNames[queryService], Templ: config.TemplateQueryService},
	} {
		t.Run("when starting "+serviceCase.Name+", it serves the component", func(t *testing.T) {
			cases := []cliutil.CommandTest{
				{
					Name:              "when configuration is valid, it starts the service",
					Args:              serviceCase.Cmd,
					CmdLoggerOutputs:  []string{"Serving", s.ThisService.GrpcEndpoint.String()},
					CmdStdOutput:      fmt.Sprintf("Starting %v", serviceCase.Name),
					UseConfigTemplate: serviceCase.Templ,
					System:            s,
				},
			}
			for _, test := range cases {
				tc := test
				t.Run(test.Name, func(t *testing.T) {
					cliutil.UnitTestRunner(t, committerCMD(), tc)
				})
			}
		})
	}
}
