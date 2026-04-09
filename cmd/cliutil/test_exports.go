/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package cliutil

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hyperledger/fabric-lib-go/common/flogging"
	commontypes "github.com/hyperledger/fabric-x-common/api/types"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hyperledger/fabric-x-committer/cmd/config"
	"github.com/hyperledger/fabric-x-committer/loadgen/workload"
	"github.com/hyperledger/fabric-x-committer/mock"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

// CommandTest is a struct that represents a CMD unit test.
type CommandTest struct {
	Name              string
	Args              []string
	CmdLoggerOutputs  []string
	CmdStdOutput      string
	CmdStdErrOutput   string
	Err               error
	UseConfigTemplate string
	System            config.SystemConfig
}

// StartDefaultSystem starts a system with mocks for CMD testing.
func StartDefaultSystem(t *testing.T) config.SystemConfig {
	t.Helper()
	serverParams := test.StartServerParameters{
		NumService: 1,
	}
	_, verifier := mock.StartMockVerifierService(t, serverParams)
	_, vc := mock.StartMockVCService(t, serverParams)
	orderer := mock.NewOrdererTestEnv(t, &mock.OrdererTestParameters{
		OrdererConfig: &mock.OrdererConfig{
			SendGenesisBlock: true,
		},
	})
	_, coordinator := mock.StartMockCoordinatorService(t, serverParams)
	server := test.NewLocalHostServer(test.InsecureTLSConfig)
	listen, err := server.Listener(t.Context())
	require.NoError(t, err)
	connection.CloseConnectionsLog(listen)

	ordererEp := orderer.AllServerConfig[0].Endpoint
	policy := &workload.PolicyProfile{
		ArtifactsPath:         t.TempDir(),
		ChannelID:             "channel1",
		OrdererEndpoints:      []*commontypes.OrdererEndpoint{{Host: ordererEp.Host, Port: ordererEp.Port}},
		PeerOrganizationCount: 1,
	}
	_, err = workload.CreateOrExtendConfigBlockWithCrypto(policy)
	require.NoError(t, err)

	return config.SystemConfig{
		ThisService: config.ServiceConfig{
			GrpcEndpoint: &server.Endpoint,
		},
		Services: config.SystemServices{
			Verifier:    []config.ServiceConfig{{GrpcEndpoint: &verifier.Configs[0].Endpoint}},
			VCService:   []config.ServiceConfig{{GrpcEndpoint: &vc.Configs[0].Endpoint}},
			Orderer:     []config.ServiceConfig{{GrpcEndpoint: &ordererEp}},
			Coordinator: config.ServiceConfig{GrpcEndpoint: &coordinator.Configs[0].Endpoint},
		},
		DB:         defaultTestDBConfig(),
		Policy:     policy,
		LedgerPath: t.TempDir(),
	}
}

// UnitTestRunner is a function that runs a unit test with the requested parameters passed in CommandTest structure.
func UnitTestRunner(
	t *testing.T,
	cmd *cobra.Command,
	cmdTest CommandTest,
) {
	t.Helper()
	// Redirect os.Stderr to a file so we can capture log output for assertions.
	// flogging defaults to os.Stderr when Config.Writer is nil. By pointing os.Stderr
	// at a file, both the initial Init here and any subsequent Init calls (e.g., from
	// ReadYamlAndSetupLogging during service startup) write to the same file.
	loggerPath := filepath.Clean(path.Join(t.TempDir(), "logger-output.txt"))
	logFile, err := os.OpenFile(loggerPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	require.NoError(t, err)
	origStderr := os.Stderr
	os.Stderr = logFile
	t.Cleanup(func() {
		os.Stderr = origStderr
		_ = logFile.Close()
	})
	testLogSpec := "debug:grpc=error"
	flogging.ActivateSpec(testLogSpec)
	t.Cleanup(func() {
		flogging.ActivateSpec("info:grpc=error")
	})
	cmdTest.System.Logging.LogSpec = testLogSpec

	args := cmdTest.Args
	if cmdTest.UseConfigTemplate != "" {
		configPath := config.CreateTempConfigFromTemplate(t, cmdTest.UseConfigTemplate, &cmdTest.System)
		args = append(args, "--config", configPath)
	}
	cmd.SetArgs(args)

	// Creating new buffers for the cmd stdout and stderr and redirect the CMD output.
	var cmdStdOut test.SafeBuffer
	var cmdStdErr test.SafeBuffer
	cmd.SetOut(&cmdStdOut)
	cmd.SetErr(&cmdStdErr)

	wg := &sync.WaitGroup{}
	t.Cleanup(func() {
		t.Log("Waiting for command to finish")
		wg.Wait()
	})

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)

	cmdDone := make(chan any)
	wg.Go(func() {
		t.Logf("Starting command: %s", args[0])
		defer t.Logf("Command exited: %s", args[0])
		defer close(cmdDone)
		_, execErr := cmd.ExecuteContextC(ctx)
		execErr = connection.FilterStreamRPCError(execErr)
		if cmdTest.Err == nil {
			assert.NoError(t, execErr)
		} else if assert.Error(t, execErr) {
			assert.Equal(t, cmdTest.Err.Error(), execErr.Error())
		}
	})

	assert.Eventually(t, func() bool {
		if len(missingExpectedOutputs(cmdTest, cmdStdOut.String(), cmdStdErr.String(), loggerPath)) == 0 {
			return true
		}
		// If the command has already exited but expected outputs are still missing,
		// fail fast — they will never appear.
		select {
		case <-cmdDone:
			t.Fatalf("Command exited before expected output appeared.\nSTD ERR: %s\nSTD OUT: %s",
				cmdStdErr.String(), cmdStdOut.String())
		default:
		}
		return false
	}, 10*time.Minute, 500*time.Millisecond)

	t.Log("Stopping command, and waiting for finish")
	cancel()
	wg.Wait()
	if !t.Failed() {
		return
	}

	t.Log("Test finished with error")
	t.Log("STD OUT:\n", cmdStdOut.String())
	t.Log("STD ERR:\n", cmdStdErr.String())
	logOut, err := os.ReadFile(loggerPath)
	if err == nil {
		t.Log("LOG:\n", string(logOut))
	}
	for _, m := range missingExpectedOutputs(cmdTest, cmdStdOut.String(), cmdStdErr.String(), loggerPath) {
		t.Logf("Missing: %s", m)
	}
}

// defaultTestDBConfig returns DB config with credentials matching the DB_TYPE env var.
// Defaults to "yugabyte" when DB_TYPE is unset (YugabyteDB), "postgres" when DB_TYPE=postgres.
func defaultTestDBConfig() config.DatabaseConfig {
	username := "yugabyte"
	if strings.EqualFold(os.Getenv("DB_TYPE"), "postgres") {
		username = "postgres"
	}
	return config.DatabaseConfig{
		Name:        "dummy_test_db",
		Username:    username,
		Endpoints:   []*connection.Endpoint{{Host: "localhost", Port: 5433}},
		LoadBalance: false,
	}
}

func missingExpectedOutputs(expected CommandTest, stdout, stderr, logFilePath string) (missing []string) {
	if expected.CmdStdOutput != "" && !strings.Contains(stdout, expected.CmdStdOutput) {
		missing = append(missing, expected.CmdStdOutput)
	}
	if expected.CmdStdErrOutput != "" && !strings.Contains(stderr, expected.CmdStdErrOutput) {
		missing = append(missing, expected.CmdStdErrOutput)
	}
	if len(expected.CmdLoggerOutputs) == 0 {
		return missing
	}
	logOut, err := os.ReadFile(logFilePath)
	if err != nil {
		return append(missing, logFilePath)
	}
	for _, loggerLine := range expected.CmdLoggerOutputs {
		if !strings.Contains(string(logOut), loggerLine) {
			missing = append(missing, loggerLine)
		}
	}
	return missing
}
