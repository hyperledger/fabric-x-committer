/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package config

import (
	"bytes"
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
	Err               error
	UseConfigTemplate string
	System            SystemConfig
}

// StartDefaultSystem starts a system with mocks for CMD testing.
func StartDefaultSystem(t *testing.T) SystemConfig {
	t.Helper()
	serverParams := test.StartServerParameters{
		NumService: 1,
	}
	_, verifier := mock.StartMockVerifierService(t, serverParams)
	_, vc := mock.StartMockVCService(t, serverParams)
	_, orderer := mock.StartMockOrderingServices(t, &mock.OrdererConfig{TestServerParameters: serverParams})
	_, coordinator := mock.StartMockCoordinatorService(t, serverParams)
	server := connection.NewLocalHostServer(test.InsecureTLSConfig)
	listen, err := server.Listener(t.Context())
	require.NoError(t, err)
	connection.CloseConnectionsLog(listen)

	ordererEp := orderer.Configs[0].Endpoint
	policy := &workload.PolicyProfile{
		CryptoMaterialPath:    t.TempDir(),
		ChannelID:             "channel1",
		OrdererEndpoints:      []*commontypes.OrdererEndpoint{{Host: ordererEp.Host, Port: ordererEp.Port}},
		PeerOrganizationCount: 1,
	}
	_, err = workload.CreateConfigBlock(policy)
	require.NoError(t, err)

	return SystemConfig{
		ThisService: ServiceConfig{
			GrpcEndpoint: &server.Endpoint,
		},
		Services: SystemServices{
			Verifier:    []ServiceConfig{{GrpcEndpoint: &verifier.Configs[0].Endpoint}},
			VCService:   []ServiceConfig{{GrpcEndpoint: &vc.Configs[0].Endpoint}},
			Orderer:     []ServiceConfig{{GrpcEndpoint: &orderer.Configs[0].Endpoint}},
			Coordinator: ServiceConfig{GrpcEndpoint: &coordinator.Configs[0].Endpoint},
		},
		DB: DatabaseConfig{
			Name:        "dummy_test_db",
			Endpoints:   []*connection.Endpoint{connection.CreateEndpointHP("localhost", "5433")},
			LoadBalance: false,
		},
		Policy:     policy,
		LedgerPath: t.TempDir(),
	}
}

// UnitTestRunner is a function that runs a unit test with the requested parameters passed in CommandTest structure.
func UnitTestRunner(
	t *testing.T,
	cmd *cobra.Command,
	test CommandTest,
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
	logConfig := &flogging.Config{
		LogSpec: "debug",
	}
	flogging.Init(*logConfig)
	test.System.Logging = logConfig

	args := test.Args
	if test.UseConfigTemplate != "" {
		configPath := CreateTempConfigFromTemplate(t, test.UseConfigTemplate, &test.System)
		args = append(args, "--config", configPath)
	}
	cmd.SetArgs(args)

	// Creating new buffers for the cmd stdout and stderr and redirect the CMD output.
	var cmdStdOut bytes.Buffer
	var cmdStdErr bytes.Buffer
	cmd.SetOut(&cmdStdOut)
	cmd.SetErr(&cmdStdErr)

	wg := &sync.WaitGroup{}
	t.Cleanup(func() {
		t.Log("Waiting for command to finish")
		defer t.Log("Command finished")
		wg.Wait()
	})

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)

	wg.Add(1)
	go func() {
		t.Logf("Starting command: %s", args[0])
		defer t.Logf("Command exited: %s", args[0])
		defer wg.Done()
		_, err := cmd.ExecuteContextC(ctx)
		err = connection.FilterStreamRPCError(err)
		if test.Err == nil {
			assert.NoError(t, err)
		} else if assert.Error(t, err) {
			assert.Equal(t, test.Err.Error(), err.Error())
		}
	}()

	assert.Eventually(t, func() bool {
		return len(getMissing(test, &cmdStdOut, loggerPath)) == 0
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
	for _, m := range getMissing(test, &cmdStdOut, loggerPath) {
		t.Logf("Missing: %s", m)
	}
}

func getMissing(test CommandTest, cmdStdOut *bytes.Buffer, loggerPath string) (missing []string) {
	if test.CmdStdOutput != "" && !strings.Contains(cmdStdOut.String(), test.CmdStdOutput) {
		missing = append(missing, test.CmdStdOutput)
	}
	if len(test.CmdLoggerOutputs) == 0 {
		return missing
	}
	logOut, err := os.ReadFile(loggerPath)
	if err != nil {
		return append(missing, loggerPath)
	}
	for _, loggerLine := range test.CmdLoggerOutputs {
		if !strings.Contains(string(logOut), loggerLine) {
			missing = append(missing, loggerLine)
		}
	}
	return missing
}
